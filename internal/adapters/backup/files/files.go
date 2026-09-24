// Package files 是文件/目录备份适配器：把策略声明的目录集合打成 tar 流，并支持
// 恢复、校验与预检。
//
// 它只产出**逻辑流**。压缩、加密、摘要与原子提交到 StorageBackend 全部由应用层负责
// （迭代 2 规格 D1）——把加密下放给每个适配器，等于让每种数据库各实现一套，
// 任何一处写错都是静默的数据泄露或静默的不可恢复。
package files

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// Tool 是写进备份元数据的工具名。
const Tool = "tar"

// Adapter 实现 application.BackupAdapter，负责 kind=files。
type Adapter struct {
	// root 是路径前缀重定向，生产传 "/"。这样无特权环境也能在临时目录里跑完整合约，
	// 断言的是真实的文件内容与模式——与 systemd 适配器的 RootPath 同一个理由：
	// 假实现的权限断言永远是测试自己造的那种关系。
	root string
	// tempRoot 是隔离恢复的落点根；空则用系统临时目录。
	tempRoot string

	mu sync.Mutex
	// temps 按策略名记下「还没被清掉的临时目录」。
	//
	// 正常路径下 Restore 会自己收尾；这里记的是**被中断**的那一次留下的东西——
	// Cleanup 是它们唯一的出口。按策略名而不是 operationID 索引，是因为端口的
	// Restore 拿不到 operationID。
	temps map[string][]string
}

// Option 用于在测试里改装配参数。
type Option func(*Adapter)

// WithRoot 覆盖路径前缀（测试用；生产用 "/"）。
func WithRoot(root string) Option {
	return func(a *Adapter) { a.root = root }
}

// WithTempRoot 覆盖隔离恢复的落点根（测试用）。
func WithTempRoot(root string) Option {
	return func(a *Adapter) { a.tempRoot = root }
}

func New(opts ...Option) *Adapter {
	adapter := &Adapter{root: "/", temps: map[string][]string{}}
	for _, opt := range opts {
		opt(adapter)
	}
	if adapter.root == "" {
		adapter.root = "/"
	}
	return adapter
}

func (a *Adapter) Kind() domain.BackupResourceKind { return domain.BackupResourceFiles }

// RootPath 把策略里的绝对路径映射到本适配器的实际前缀之下。导出它是为了让测试
// 断言真实产物，而不是复制一份前缀规则——复制出来的规则迟早会与实现分叉。
func (a *Adapter) RootPath(p string) string {
	return filepath.Join(a.root, filepath.FromSlash(p))
}

func (a *Adapter) Validate(_ context.Context, policy *domain.BackupPolicy) error {
	if policy == nil {
		return domain.NewError(v1.CodeInvalidRequest, "files 适配器需要非空策略")
	}
	if policy.Resource.Kind != domain.BackupResourceFiles {
		return domain.NewError(v1.CodeInvalidRequest,
			"files 适配器只处理 kind=files，got %q", policy.Resource.Kind)
	}
	if err := policy.Validate(); err != nil {
		return err
	}
	// 排除模式在提交期就该是合法的 glob，不能等到遍历到某个文件才发现写错了。
	for _, pattern := range policy.Resource.Exclude {
		if _, err := path.Match(pattern, ""); err != nil {
			return domain.NewError(v1.CodeManifestInvalid, "resource.exclude 模式 %q 非法: %v", pattern, err)
		}
	}
	return nil
}

// Preflight 检查每个声明路径是否存在且可读，并检查排除模式是否合法。
// 它**不产生任何备份产物**：预检的意义正是在产生产物之前把问题暴露出来。
func (a *Adapter) Preflight(ctx context.Context, policy *domain.BackupPolicy) (domain.PreflightReport, error) {
	if err := a.Validate(ctx, policy); err != nil {
		return domain.PreflightReport{}, err
	}

	report := domain.PreflightReport{ToolVersion: Tool}
	report.Checks = append(report.Checks, domain.PreflightCheck{
		Name: "excludePatterns", OK: true,
		Detail: fmt.Sprintf("%d 个排除模式", len(policy.Resource.Exclude)),
	})

	readable := true
	var missing []string
	for _, declared := range policy.Resource.Paths {
		info, err := os.Stat(a.RootPath(declared))
		switch {
		case err != nil:
			readable = false
			missing = append(missing, declared)
		case !info.IsDir() && !info.Mode().IsRegular():
			// 设备、socket 之类不是「文件备份」能处理的东西，早点说清楚，
			// 而不是遍历到一半才失败。
			readable = false
			missing = append(missing, declared+"（不是普通文件或目录）")
		}
		if err := ctx.Err(); err != nil {
			return domain.PreflightReport{}, domain.NewError(v1.CodeExecCancelled, "预检被取消")
		}
	}
	report.Checks = append(report.Checks, domain.PreflightCheck{
		Name: "pathsReadable",
		OK:   readable,
		Detail: func() string {
			if readable {
				return fmt.Sprintf("%d 个路径可读", len(policy.Resource.Paths))
			}
			return "不可读: " + strings.Join(missing, ", ")
		}(),
	})

	report.Checks = append(report.Checks, domain.PreflightCheck{
		Name: "encryption", OK: true,
		Detail: func() string {
			if policy.Encoding.Encryption.Enabled {
				// 只报告引用名，不解析、不打印密钥本身。
				return "已启用，密钥引用 " + policy.Encoding.Encryption.KeySecret.String()
			}
			return "未启用（备份将以明文存放）"
		}(),
	})

	if !report.Passed() {
		check, _ := report.Failure()
		return report, domain.NewError(v1.CodeBackupPreflightFailed,
			"预检未通过（%s）：%s", check.Name, check.Detail)
	}
	return report, nil
}

// Backup 把策略声明的目录写成 tar 流。
//
// 归档里的条目名是**去掉了前导斜杠的绝对路径**（`/var/lib/x/a` → `var/lib/x/a`）。
// 这样恢复时既能放回原位（前缀 "/"），也能落到隔离目录（前缀换成临时根），
// 而条目名里永远没有绝对路径——绝对路径条目是 tar 解包最经典的一类路径穿越。
func (a *Adapter) Backup(ctx context.Context, policy *domain.BackupPolicy, w io.Writer) (domain.BackupMetadata, error) {
	if err := a.Validate(ctx, policy); err != nil {
		return domain.BackupMetadata{}, err
	}

	writer := tar.NewWriter(w)
	for _, declared := range policy.Resource.Paths {
		if err := ctx.Err(); err != nil {
			return domain.BackupMetadata{}, domain.NewError(v1.CodeExecCancelled, "备份被取消")
		}
		if err := a.writeTree(ctx, writer, policy, declared); err != nil {
			return domain.BackupMetadata{}, err
		}
	}
	if err := writer.Close(); err != nil {
		return domain.BackupMetadata{}, domain.NewError(v1.CodeInternal, "写入备份流失败: %v", err)
	}

	return domain.BackupMetadata{
		ResourceKind: domain.BackupResourceFiles,
		Tool:         Tool,
	}, nil
}

func (a *Adapter) writeTree(ctx context.Context, writer *tar.Writer, policy *domain.BackupPolicy, declared string) error {
	actualRoot := a.RootPath(declared)

	// 用 WalkDir 的字典序保证归档条目顺序稳定：同一份内容两次备份应当产出同样的字节，
	// 否则「内容寻址」的 digest 每次都不一样，去重与比对都无从谈起。
	return filepath.WalkDir(actualRoot, func(actual string, entry fs.DirEntry, err error) error {
		if err != nil {
			return domain.NewError(v1.CodeInternal, "遍历 %s 失败: %v", actual, err)
		}
		if err := ctx.Err(); err != nil {
			return domain.NewError(v1.CodeExecCancelled, "备份被取消")
		}

		// 把实际路径换回策略里的逻辑路径，排除模式与归档条目名都按逻辑路径算。
		logical, relErr := a.logicalPath(declared, actualRoot, actual)
		if relErr != nil {
			return relErr
		}
		if logical != declared && a.excluded(declared, logical, policy.Resource.Exclude) {
			if entry.IsDir() {
				// 命中排除规则的目录连子树一起跳过——否则 `cache` 这类模式
				// 只能排除目录本身，里面的文件照样进归档。
				return filepath.SkipDir
			}
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return domain.NewError(v1.CodeInternal, "读取 %s 的信息失败: %v", actual, err)
		}
		switch {
		case entry.IsDir():
			return writer.WriteHeader(&tar.Header{
				Typeflag: tar.TypeDir,
				Name:     entryName(logical),
				Mode:     int64(info.Mode().Perm()),
				ModTime:  info.ModTime(),
				Format:   tar.FormatPAX,
			})
		case info.Mode().IsRegular():
			return a.writeFile(writer, actual, logical, info)
		case info.Mode()&os.ModeSymlink != 0:
			return a.writeSymlink(ctx, writer, policy, actual, logical)
		default:
			// 设备、socket、fifo：不是「文件备份」该搬的东西。静默跳过比报错更危险
			// ——用户会以为备份是全的。
			return domain.NewError(v1.CodeBackupPreflightFailed,
				"%s 不是普通文件或目录（mode=%s），file 备份不支持", logical, info.Mode())
		}
	})
}

func (a *Adapter) writeFile(writer *tar.Writer, actual, logical string, info fs.FileInfo) error {
	file, err := os.Open(actual)
	if err != nil {
		return domain.NewError(v1.CodeInternal, "打开 %s 失败: %v", logical, err)
	}
	defer file.Close()

	if err := writer.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     entryName(logical),
		Mode:     int64(info.Mode().Perm()),
		Size:     info.Size(),
		ModTime:  info.ModTime(),
		Format:   tar.FormatPAX,
	}); err != nil {
		return domain.NewError(v1.CodeInternal, "写入归档头失败: %v", err)
	}
	if _, err := io.Copy(writer, file); err != nil {
		return domain.NewError(v1.CodeInternal, "写入 %s 的内容失败: %v", logical, err)
	}
	return nil
}

// writeSymlink 按策略处理符号链接。
//
// follow 只支持指向**普通文件**的链接：指向目录的链接会引入成环的可能，而一个能绕开
// paths 范围、还可能自指的备份，比「不支持」危险得多。要跟随目录请把目标直接写进 paths。
func (a *Adapter) writeSymlink(ctx context.Context, writer *tar.Writer, policy *domain.BackupPolicy, actual, logical string) error {
	switch policy.Resource.Symlinks {
	case domain.SymlinkSkip:
		return nil
	case domain.SymlinkError:
		return domain.NewError(v1.CodeBackupPreflightFailed,
			"%s 是符号链接，而 resource.symlinks=error", logical)
	case domain.SymlinkFollow:
		target, err := filepath.EvalSymlinks(actual)
		if err != nil {
			return domain.NewError(v1.CodeInternal, "解析 %s 的链接目标失败: %v", logical, err)
		}
		info, err := os.Stat(target)
		if err != nil {
			return domain.NewError(v1.CodeInternal, "读取 %s 的链接目标失败: %v", logical, err)
		}
		if !info.Mode().IsRegular() {
			return domain.NewError(v1.CodeBackupPreflightFailed,
				"%s 指向的不是普通文件（mode=%s）；跟随目录链接会绕开 paths 范围，请直接把目标写进 paths",
				logical, info.Mode())
		}
		// 归档里记的是**链接名 + 目标的内容**：恢复出来的是一份普通文件，
		// 而不是一个还指着别处的链接——备份的语义就是「把内容拿走」。
		return a.writeFile(writer, target, logical, info)
	default:
		return domain.NewError(v1.CodeManifestInvalid, "resource.symlinks 取值非法: %q", policy.Resource.Symlinks)
	}
}

// Verify 只读一遍归档的结构，**不接触任何目标**。
//
// 它是「这份备份能不能恢复」的第一道、也是最便宜的一道证据：条目头的解析、长度的
// 自洽都在这条路径上，但它证明不了内容真的能放回去——那需要隔离恢复。
func (a *Adapter) Verify(ctx context.Context, policy *domain.BackupPolicy, r io.Reader) error {
	if err := a.Validate(ctx, policy); err != nil {
		return err
	}

	reader := tar.NewReader(r)
	count := 0
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return domain.NewError(v1.CodeBackupVerifyFailed, "归档在读取过程中损坏: %v", err)
		}
		if err := ctx.Err(); err != nil {
			return domain.NewError(v1.CodeExecCancelled, "校验被取消")
		}
		if err := validateEntryName(header.Name); err != nil {
			return err
		}
		count++
	}
	if count == 0 {
		// 空归档能「通过校验」但不能恢复任何东西。对备份来说，空 =
		// 静默的不可恢复，比报错更危险。
		return domain.NewError(v1.CodeBackupVerifyFailed, "归档里没有任何条目")
	}
	return nil
}

// Restore 把 tar 流解出来。
//
//   - isolated：解到一个临时根之下，完成后立刻销毁，**绝不碰真实路径**。
//   - inPlace：解到真实路径。这是本工具里破坏性最强的动作，调用方必须先做显式确认
//     （确认在应用层，见规格 D7）。
func (a *Adapter) Restore(ctx context.Context, policy *domain.BackupPolicy, r io.Reader, mode domain.RestoreMode) error {
	if err := a.Validate(ctx, policy); err != nil {
		return err
	}
	if !mode.Valid() {
		return domain.NewError(v1.CodeInvalidRequest, "恢复模式取值非法: %q", mode)
	}

	destRoot := a.root
	var tempDir string
	if mode == domain.RestoreIsolated {
		dir, err := os.MkdirTemp(a.tempRoot, "frz-backup-restore-")
		if err != nil {
			return domain.NewError(v1.CodeInternal, "无法创建隔离恢复目录: %v", err)
		}
		tempDir = dir
		destRoot = dir
		a.trackTemp(policy.Name, dir)
	}

	err := a.extract(ctx, r, destRoot)
	if tempDir != "" {
		// 无论成败都销毁隔离目录：成功时它已经没用了，失败时留下的是一份
		// 不一致的半成品，留着只会误导。
		_ = os.RemoveAll(tempDir)
		a.untrackTemp(policy.Name, tempDir)
	}
	return err
}

func (a *Adapter) extract(ctx context.Context, r io.Reader, destRoot string) error {
	reader := tar.NewReader(r)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return domain.NewError(v1.CodeBackupVerifyFailed, "归档在读取过程中损坏: %v", err)
		}
		if err := ctx.Err(); err != nil {
			return domain.NewError(v1.CodeExecCancelled, "恢复被取消")
		}
		if err := validateEntryName(header.Name); err != nil {
			return err
		}

		target := filepath.Join(destRoot, filepath.FromSlash(header.Name))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, fs.FileMode(header.Mode).Perm()); err != nil {
				return domain.NewError(v1.CodeInternal, "创建目录 %s 失败: %v", header.Name, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return domain.NewError(v1.CodeInternal, "创建目录 %s 失败: %v", filepath.Dir(target), err)
			}
			// O_TRUNC：恢复是「让内容等于归档」，而不是「往现有文件里追加」。
			file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fs.FileMode(header.Mode).Perm())
			if err != nil {
				return domain.NewError(v1.CodeInternal, "打开 %s 失败: %v", target, err)
			}
			if _, err := io.Copy(file, reader); err != nil {
				file.Close()
				return domain.NewError(v1.CodeInternal, "写入 %s 失败: %v", target, err)
			}
			if err := file.Close(); err != nil {
				return domain.NewError(v1.CodeInternal, "关闭 %s 失败: %v", target, err)
			}
		default:
			return domain.NewError(v1.CodeBackupVerifyFailed,
				"归档里有不支持的条目类型 %q（%s）", header.Typeflag, header.Name)
		}
	}
}

// Cleanup 删掉本策略下**还没收尾**的临时目录。
//
// 幂等：没有临时目录、或已经被删过，都返回成功——它会在中断路径上被反复调用。
func (a *Adapter) Cleanup(_ context.Context, policy *domain.BackupPolicy, _ string) error {
	if policy == nil {
		return nil
	}

	a.mu.Lock()
	dirs := a.temps[policy.Name]
	delete(a.temps, policy.Name)
	a.mu.Unlock()

	var firstErr error
	for _, dir := range dirs {
		if err := os.RemoveAll(dir); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return domain.NewError(v1.CodeInternal, "清理临时目录失败: %v", firstErr)
	}
	return nil
}

func (a *Adapter) trackTemp(policyName, dir string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.temps[policyName] = append(a.temps[policyName], dir)
}

func (a *Adapter) untrackTemp(policyName, dir string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// 刻意新建切片而不是复用底层数组：在 a.temps[policyName] 上切片再 append
	// 会与正在遍历的那个切片别名，边读边写同一个底层数组。
	var kept []string
	for _, existing := range a.temps[policyName] {
		if existing != dir {
			kept = append(kept, existing)
		}
	}
	if len(kept) == 0 {
		delete(a.temps, policyName)
		return
	}
	a.temps[policyName] = kept
}

// logicalPath 把实际路径换回策略里的逻辑路径。
func (a *Adapter) logicalPath(declared, actualRoot, actual string) (string, error) {
	rel, err := filepath.Rel(actualRoot, actual)
	if err != nil {
		return "", domain.NewError(v1.CodeInternal, "无法计算 %s 的相对路径: %v", actual, err)
	}
	if rel == "." {
		return declared, nil
	}
	return path.Join(declared, filepath.ToSlash(rel)), nil
}

// excluded 判断一条逻辑路径是否命中排除规则。
//
// 模式按 `path.Match` 的语义匹配**相对声明根的路径**或**基名**两种写法：
// `*.tmp` 匹配任意层级的 .tmp 文件，`data/tmp` 匹配 data 下的 tmp。
// 目录命中时调用方会整棵子树跳过。
func (a *Adapter) excluded(declared, logical string, patterns []string) bool {
	rel := strings.TrimPrefix(logical, declared)
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" {
		return false
	}
	base := path.Base(rel)

	candidates := []string{rel, base}
	// 排序只为让行为可预期，与匹配顺序无关。
	sort.Strings(candidates)

	for _, pattern := range patterns {
		for _, candidate := range candidates {
			if ok, err := path.Match(pattern, candidate); err == nil && ok {
				return true
			}
		}
	}
	return false
}

// entryName 把逻辑路径转成归档条目名：去掉前导斜杠。
// 归档里永远不出现绝对路径，也不出现 ..。
func entryName(logical string) string {
	return strings.TrimPrefix(logical, "/")
}

// validateEntryName 拒绝绝对路径与 .. 这类条目名。
//
// 这是**解包侧**的最后一道防线：即使归档是别人给的，也不能让它写到目标根之外。
func validateEntryName(name string) error {
	if name == "" {
		return domain.NewError(v1.CodeBackupVerifyFailed, "归档里有空条目名")
	}
	if path.IsAbs(name) || strings.HasPrefix(name, "/") {
		return domain.NewError(v1.CodeBackupVerifyFailed, "归档条目名不得是绝对路径: %q", name)
	}
	clean := path.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return domain.NewError(v1.CodeBackupVerifyFailed, "归档条目名逃出了目标根: %q", name)
	}
	return nil
}
