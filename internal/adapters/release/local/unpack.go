package local

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// 下面这套校验是**解包的全部安全边界**。它们挡的攻击有个统一的名字：tar slip / zip slip
// ——归档里放一个指向目录之外的条目（绝对路径、`..`、或者先放一个符号链接再往里写），
// 解包方跟着走就会把文件写到 release 目录之外。三条一起做，缺一条都不够：
//
//  1. 条目名逐条校验（绝对路径、`..` 一律拒绝）；
//  2. 写文件之前，**逐级检查父目录链**，任何一级是符号链接就拒绝穿过（这一条挡的是
//     「先放 link -> /etc，再放 link/shadow」）；
//  3. 目标同名已存在且是符号链接时拒绝写入（这一条挡的是「先放 link，再放同名文件」，
//     否则 O_CREATE 会跟随链接写到它指向的地方）。
//
// 硬链接额外一条：它的目标必须在 release 目录之内。硬链接是「同一个 inode 的第二个名字」，
// 指向外面等于把宿主文件变成 release 的一部分。
//
// 符号链接的**目标**允许指向外面（共享库、/etc 下的配置是正常用法）：上面第 2、3 条
// 保证我们不会跟着它走，而它本身只是一个名字。

// unpackArtifact 按规格声明的策略把制品字节解到 dest。
func unpackArtifact(ctx context.Context, spec *domain.ApplicationSpec, artifact io.Reader, dest string, owner ownership) error {
	extractor := &extractor{dest: dest, owner: owner}
	unpack := spec.Artifact.Unpack

	switch unpack.Strategy {
	case domain.UnpackNone:
		return extractor.singleFile(ctx, spec, artifact)
	case domain.UnpackTar:
		return extractor.tarStream(ctx, artifact, unpack.StripComponents)
	case domain.UnpackTarGz:
		reader, err := gzip.NewReader(artifact)
		if err != nil {
			return domain.NewError(v1.CodeArtifactUnpackFailed, "制品不是合法的 gzip 流: %v", err)
		}
		defer reader.Close()
		return extractor.tarStream(ctx, reader, unpack.StripComponents)
	case domain.UnpackZip:
		return extractor.zipStream(ctx, artifact, unpack.StripComponents)
	default:
		return domain.NewError(v1.CodeManifestInvalid, "unpack.strategy 取值非法: %q", unpack.Strategy)
	}
}

type extractor struct {
	dest  string
	owner ownership
}

// singleFile 处理 unpack.strategy=none：制品是一个单文件（Go 二进制、单个 JAR）。
//
// 落点由 domain 决定（artifact.fileName，缺省取相对的 argv[0]）——单文件制品没有归档
// 自带的名字，而「叫什么」决定了 argv 里怎么写它。**返回空串表示这份 manifest 不需要
// 物化任何东西**（argv[0] 指向发布目录之外既有程序的那种形态），此时什么都不做。
func (e *extractor) singleFile(ctx context.Context, spec *domain.ApplicationSpec, artifact io.Reader) error {
	name := spec.MaterializedFileName()
	if name == "" {
		return nil
	}
	return e.writeFile(ctx, name, singleFileMode, artifact)
}

// tarStream 解一个 tar 流。
//
// 链接条目**先记下来、最后统一建**：两趟之外还有个更省事的做法是「边解边建、但绝不穿过
// 已有的链接」（上面的第 2、3 条已经保证了这一点），因此这里不需要把归档缓冲两遍。
// 延迟建链接让顺序问题彻底消失，代价只是多一个切片。
func (e *extractor) tarStream(ctx context.Context, r io.Reader, strip int) error {
	reader := tar.NewReader(r)
	var pendingLinks []pendingLink

	for {
		if err := ctx.Err(); err != nil {
			return domain.NewError(v1.CodeExecCancelled, "解包被取消")
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return domain.NewError(v1.CodeArtifactUnpackFailed, "读取 tar 归档失败: %v", err)
		}

		rel, ok, err := cleanEntryName(header.Name, strip)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := e.makeDir(rel, fs.FileMode(header.Mode).Perm()); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := e.writeFile(ctx, rel, normalizeMode(fs.FileMode(header.Mode).Perm()), reader); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// 符号链接的**目标**原样保留，不做条目名校验：目标允许是绝对路径、允许含
			// `..`——指向 /etc 下的配置或系统共享库是正常用法，而它只是一个名字。
			// 我们不跟着它走（ensureDir 与 writeFile 各挡一道），所以它不构成逃逸。
			pendingLinks = append(pendingLinks, pendingLink{
				rel:    rel,
				target: header.Linkname,
			})
		case tar.TypeLink:
			// 硬链接不同：它是「同一个 inode 的第二个名字」，目标必须在 release 之内，
			// 因此目标名要按归档条目名的同一套规则清洗与校验。
			targetRel, ok, err := cleanEntryName(header.Linkname, strip)
			if err != nil {
				return err
			}
			if !ok {
				return domain.NewError(v1.CodeArtifactUnpackFailed,
					"硬链接条目 %q 的目标 %q 剥掉 %d 层后为空", header.Name, header.Linkname, strip)
			}
			pendingLinks = append(pendingLinks, pendingLink{
				rel:      rel,
				target:   header.Linkname,
				targetC:  targetRel,
				hardlink: true,
			})
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
			// pax 扩展头由 archive/tar 自己消费，走到这里说明是孤立的头，跳过。
		default:
			// 设备、fifo、socket：release 目录里不该有这些东西，而且创建它们需要额外权限。
			// 静默跳过会让「制品里有东西没解出来」变成看不见的事实，因此报错。
			return domain.NewError(v1.CodeArtifactUnpackFailed,
				"归档条目 %q 的类型 %q 不受支持（release 目录只放普通文件、目录与链接）",
				header.Name, header.Typeflag)
		}
	}
	return e.buildLinks(pendingLinks)
}

// zipStream 解一个 zip 流。JAR 也是 zip，因此这条路同样服务 Java 的发行包。
func (e *extractor) zipStream(ctx context.Context, r io.Reader, strip int) error {
	// zip 需要可寻址的输入，而制品是从存储里流式读出来的：先落到临时文件。
	// 制品大小上限由制品存储的配额约束，这里不做二次限制。
	tmp, err := os.CreateTemp(e.dest, ".artifact-*.zip")
	if err != nil {
		return domain.NewError(v1.CodeInternal, "创建 zip 临时文件失败: %v", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpPath)
	}()

	size, err := io.Copy(tmp, r)
	if err != nil {
		return domain.NewError(v1.CodeArtifactUnpackFailed, "读取制品失败: %v", err)
	}

	reader, err := zip.NewReader(tmp, size)
	if err != nil {
		return domain.NewError(v1.CodeArtifactUnpackFailed, "制品不是合法的 zip 归档: %v", err)
	}

	var pendingLinks []pendingLink
	for _, file := range reader.File {
		if err := ctx.Err(); err != nil {
			return domain.NewError(v1.CodeExecCancelled, "解包被取消")
		}
		rel, ok, err := cleanEntryName(file.Name, strip)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}

		mode := file.Mode()
		if mode.IsDir() {
			if err := e.makeDir(rel, normalizeMode(mode.Perm())); err != nil {
				return err
			}
			continue
		}

		source, err := file.Open()
		if err != nil {
			return domain.NewError(v1.CodeArtifactUnpackFailed, "打开 zip 条目 %q 失败: %v", file.Name, err)
		}
		switch {
		case mode&os.ModeSymlink != 0:
			// zip 里的符号链接把目标写在内容里。
			target, readErr := io.ReadAll(io.LimitReader(source, 4096))
			source.Close()
			if readErr != nil {
				return domain.NewError(v1.CodeArtifactUnpackFailed, "读取链接条目 %q 失败: %v", file.Name, readErr)
			}
			pendingLinks = append(pendingLinks, pendingLink{
				rel:    rel,
				target: string(target),
			})
		default:
			writeErr := e.writeFile(ctx, rel, normalizeMode(mode.Perm()), source)
			source.Close()
			if writeErr != nil {
				return writeErr
			}
		}
	}
	return e.buildLinks(pendingLinks)
}

// pendingLink 是一个待建的链接条目。
type pendingLink struct {
	rel      string
	target   string // 符号链接的目标原文 / 硬链接的目标条目名
	targetC  string // 清洗后的目标（硬链接用；符号链接不用清洗，目标允许指向外面）
	hardlink bool
}

// buildLinks 在所有普通文件落盘之后统一建链接。
func (e *extractor) buildLinks(links []pendingLink) error {
	for _, link := range links {
		target := filepath.Join(e.dest, link.rel)
		if err := e.ensureDir(filepath.Dir(link.rel)); err != nil {
			return err
		}
		if _, err := os.Lstat(target); err == nil {
			return domain.NewError(v1.CodeArtifactUnpackFailed,
				"链接条目 %q 与归档里已有的名字冲突", link.rel)
		}

		if link.hardlink {
			// 硬链接的目标**必须在 release 目录之内**：它是同一个 inode 的第二个名字，
			// 指到外面等于把宿主文件变成 release 的一部分。
			source := filepath.Join(e.dest, link.targetC)
			if err := e.writeHardlink(target, source, link.rel); err != nil {
				return err
			}
			continue
		}
		if err := os.Symlink(link.target, target); err != nil {
			return domain.NewError(v1.CodeArtifactUnpackFailed, "创建符号链接 %q 失败: %v", link.rel, err)
		}
		if err := e.owner.chown(target); err != nil {
			return err
		}
	}
	return nil
}

// makeDir 建一个目录条目。
func (e *extractor) makeDir(rel string, mode fs.FileMode) error {
	if err := e.ensureDir(rel); err != nil {
		return err
	}
	target := filepath.Join(e.dest, rel)
	// MkdirAll 不改已存在目录的模式，因此显式收敛一次（与 systemd 适配器同一个理由：
	// 上一次留下的宽权限会一直「看起来是对的」）。
	if err := os.Chmod(target, normalizeMode(mode)); err != nil {
		return domain.NewError(v1.CodeArtifactUnpackFailed, "收敛目录 %q 权限失败: %v", rel, err)
	}
	return e.owner.chown(target)
}

// writeFile 写一个普通文件条目。
func (e *extractor) writeFile(ctx context.Context, rel string, mode fs.FileMode, source io.Reader) error {
	if err := e.ensureDir(filepath.Dir(rel)); err != nil {
		return err
	}
	target := filepath.Join(e.dest, rel)

	// 目标同名已存在且是符号链接时**必须拒绝**：O_CREATE 会跟随链接，于是内容被写到
	// 链接指向的地方（很可能是 release 目录之外）。
	if info, err := os.Lstat(target); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return domain.NewError(v1.CodeArtifactUnpackFailed,
			"条目 %q 落点的位置已存在符号链接，拒绝跟随它写入", rel)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return domain.NewError(v1.CodeArtifactUnpackFailed, "创建目录 %q 失败: %v", filepath.Dir(rel), err)
	}

	file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, normalizeMode(mode))
	if err != nil {
		return domain.NewError(v1.CodeArtifactUnpackFailed, "创建 %q 失败: %v", rel, err)
	}
	if _, err := io.Copy(file, source); err != nil {
		file.Close()
		return domain.NewError(v1.CodeArtifactUnpackFailed, "写入 %q 失败: %v", rel, err)
	}
	if err := file.Close(); err != nil {
		return domain.NewError(v1.CodeArtifactUnpackFailed, "关闭 %q 失败: %v", rel, err)
	}
	// umask 会削掉 OpenFile 给的权限位，因此显式收敛一次。
	if err := os.Chmod(target, normalizeMode(mode)); err != nil {
		return domain.NewError(v1.CodeArtifactUnpackFailed, "收敛 %q 权限失败: %v", rel, err)
	}
	return e.owner.chown(target)
}

// writeHardlink 建一个硬链接，并确认源在 release 目录之内。
func (e *extractor) writeHardlink(target, source, rel string) error {
	// source 由 cleanEntryName 清洗过，因此一定在 dest 之内；这里再确认一次，
	// 因为它是「同一个 inode 的第二个名字」——错了就是把宿主文件搬进 release。
	if source != e.dest && !strings.HasPrefix(source, e.dest+string(filepath.Separator)) {
		return domain.NewError(v1.CodeArtifactUnpackFailed,
			"硬链接 %q 的目标逃出了 release 目录", rel)
	}
	if err := os.Link(source, target); err != nil {
		return domain.NewError(v1.CodeArtifactUnpackFailed,
			"创建硬链接 %q → %q 失败: %v（目标必须是归档里已经出现的文件）", rel, source, err)
	}
	return e.owner.chown(target)
}

// ensureDir 逐级创建目录，**任何一级已存在时都必须是目录而不是符号链接**。
//
// 这就是 tar slip 的核心防线：如果归档先放一个 `link -> /etc` 的符号链接、再放
// `link/shadow`，那么 os.MkdirAll 会高高兴兴地穿过链接，而这个函数会在这里停下。
func (e *extractor) ensureDir(rel string) error {
	if rel == "" || rel == "." {
		return nil
	}
	current := e.dest
	for _, segment := range strings.Split(filepath.ToSlash(rel), "/") {
		if segment == "" || segment == "." {
			continue
		}
		current = filepath.Join(current, segment)

		info, err := os.Lstat(current)
		switch {
		case err == nil:
			if info.Mode()&os.ModeSymlink != 0 {
				return domain.NewError(v1.CodeArtifactUnpackFailed,
					"路径 %q 已存在且是符号链接，拒绝穿过它（tar slip）", current)
			}
			if !info.IsDir() {
				return domain.NewError(v1.CodeArtifactUnpackFailed,
					"路径 %q 已存在且不是目录", current)
			}
		case os.IsNotExist(err):
			if mkErr := os.Mkdir(current, 0o755); mkErr != nil && !os.IsExist(mkErr) {
				return domain.NewError(v1.CodeArtifactUnpackFailed, "创建目录 %q 失败: %v", current, mkErr)
			}
		default:
			return domain.NewError(v1.CodeArtifactUnpackFailed, "检查路径 %q 失败: %v", current, err)
		}
	}
	return nil
}

// cleanEntryName 把归档条目名清洗成相对 dest 的干净路径。
//
// 第二个返回值表示「剥掉前 N 层之后什么都不剩，跳过这个条目」——GNU tar 的语义就是如此
// （`--strip-components` 把顶层目录整个剥掉时，目录条目自己会变成空）。
func cleanEntryName(raw string, strip int) (string, bool, error) {
	if raw == "" {
		return "", false, domain.NewError(v1.CodeArtifactUnpackFailed, "归档里有空条目名")
	}
	// Windows 风格的归档能带反斜杠；它在 POSIX 上只是普通字符，但会绕过按 `/` 切分的
	// 逐级检查。明确拒绝，而不是假装它会以别的方式被处理。
	if strings.Contains(raw, `\`) {
		return "", false, domain.NewError(v1.CodeArtifactUnpackFailed,
			"归档条目名 %q 含反斜杠，拒绝解包", raw)
	}
	if path.IsAbs(raw) || strings.HasPrefix(raw, "/") {
		return "", false, domain.NewError(v1.CodeArtifactUnpackFailed,
			"归档条目名不得是绝对路径: %q", raw)
	}

	segments := strings.Split(raw, "/")
	if strip > 0 {
		if strip >= len(segments) {
			return "", false, nil
		}
		segments = segments[strip:]
	}

	cleaned := path.Clean(strings.Join(segments, "/"))
	if cleaned == "." || cleaned == "" {
		return "", false, nil
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", false, domain.NewError(v1.CodeArtifactUnpackFailed,
			"归档条目名逃出了 release 目录: %q", raw)
	}
	if strings.HasPrefix(cleaned, "/") {
		return "", false, domain.NewError(v1.CodeArtifactUnpackFailed,
			"归档条目名不得是绝对路径: %q", raw)
	}
	return cleaned, true, nil
}

// normalizeMode 去掉非常规位，并保证**属主自己**至少能读。
//
// 第二件事是有意的：解出来的文件随后会 chown 给运行用户，而一个 0000 的文件对属主也不
// 可读——那会让「解包成功」与「应用读不到自己的文件」同时成立，是最难查的一类部署失败。
func normalizeMode(mode fs.FileMode) fs.FileMode {
	perm := mode.Perm()
	if perm&0o400 == 0 {
		perm |= 0o400
	}
	if mode&0o111 != 0 {
		perm |= 0o100
	}
	return perm
}
