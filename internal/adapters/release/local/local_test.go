package local

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// 这一组用例护的是「唯一会把外部字节变成可执行文件」的那个动作。每一条路径穿越的用例都
// 会在目标目录之外放一个**哨兵路径**，断言它没有出现——只断言「返回了错误」是不够的：
// 一个先写坏再报错的实现同样会返回错误。

func testOwner(_ string) (int, int, error) { return os.Getuid(), os.Getgid(), nil }

func newTestAdapter(t *testing.T) (*Adapter, string) {
	t.Helper()
	root := t.TempDir()
	return New(WithRoot(root), WithOwnerResolver(testOwner)), root
}

func goSpec(unpack domain.SpecUnpack, fileName string) *domain.ApplicationSpec {
	return &domain.ApplicationSpec{
		APIVersion:  domain.ManifestAPIVersion,
		Kind:        domain.ManifestKind,
		Application: "orders-api",
		Runtime:     domain.RuntimeKindGo,
		Artifact:    domain.SpecArtifact{ID: "art_1", Version: "1.0.0", FileName: fileName, Unpack: unpack},
		Exec: domain.SpecExec{
			Argv:             []string{"bin/server"},
			WorkingDirectory: "/var/lib/orders-api",
			RunUser:          "orders-api",
		},
		Health: domain.SpecHealth{Readiness: domain.SpecReadiness{Type: domain.ReadinessTCP, Target: "127.0.0.1:8080"}},
		Logs:   domain.SpecLogs{Directory: "/var/log/orders-api"},
	}
}

// releaseDir 返回某个 release 在测试根前缀下的真实路径。
func releaseDir(t *testing.T, adapter *Adapter, spec *domain.ApplicationSpec, releaseID string) string {
	t.Helper()
	return filepath.Join(adapter.RootPath(domain.ReleaseRootDir(spec.Application)), releaseID)
}

// ==== 归档构造 ====

type tarEntry struct {
	name     string
	typeflag byte
	mode     int64
	body     string
	linkname string
}

func buildTar(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	for _, entry := range entries {
		mode := entry.mode
		if mode == 0 {
			mode = 0o644
		}
		header := &tar.Header{
			Name:     entry.name,
			Typeflag: entry.typeflag,
			Mode:     mode,
			Linkname: entry.linkname,
			Size:     int64(len(entry.body)),
			Format:   tar.FormatPAX,
		}
		if entry.typeflag == tar.TypeDir {
			header.Size = 0
		}
		if entry.typeflag == tar.TypeSymlink || entry.typeflag == tar.TypeLink {
			header.Size = 0
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatalf("写 tar 头 %q: %v", entry.name, err)
		}
		if header.Size > 0 {
			if _, err := writer.Write([]byte(entry.body)); err != nil {
				t.Fatalf("写 tar 内容 %q: %v", entry.name, err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭 tar: %v", err)
	}
	return buf.Bytes()
}

type zipEntry struct {
	name string
	body string
	mode os.FileMode
}

func buildZip(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		if entry.mode != 0 {
			header.SetMode(entry.mode)
		}
		part, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatalf("写 zip 条目 %q: %v", entry.name, err)
		}
		if _, err := part.Write([]byte(entry.body)); err != nil {
			t.Fatalf("写 zip 内容 %q: %v", entry.name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭 zip: %v", err)
	}
	return buf.Bytes()
}

// ==== unpack.strategy=none ====

func TestMaterializeSingleFile(t *testing.T) {
	adapter, _ := newTestAdapter(t)
	spec := goSpec(domain.SpecUnpack{Strategy: domain.UnpackNone}, "bin/server")

	if err := adapter.Materialize(context.Background(), spec, "rel_1", strings.NewReader("binary-bytes")); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	dir := releaseDir(t, adapter, spec, "rel_1")
	content, err := os.ReadFile(filepath.Join(dir, "bin", "server"))
	if err != nil {
		t.Fatalf("读取落盘的单文件: %v", err)
	}
	if string(content) != "binary-bytes" {
		t.Fatalf("内容不对: %q", content)
	}
	info, err := os.Stat(filepath.Join(dir, "bin", "server"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("单文件制品应当可执行：want 0755, got %04o", info.Mode().Perm())
	}
	// release 目录本身按 1c 决定 4：0750。
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat release 目录: %v", err)
	}
	if dirInfo.Mode().Perm() != 0o750 {
		t.Fatalf("release 目录模式 want 0750, got %04o", dirInfo.Mode().Perm())
	}
}

func TestMaterializeRejectsExistingReleaseDir(t *testing.T) {
	adapter, _ := newTestAdapter(t)
	spec := goSpec(domain.SpecUnpack{Strategy: domain.UnpackNone}, "server")

	if err := adapter.Materialize(context.Background(), spec, "rel_1", strings.NewReader("first")); err != nil {
		t.Fatalf("第一次 Materialize: %v", err)
	}
	err := adapter.Materialize(context.Background(), spec, "rel_1", strings.NewReader("second"))
	if domain.CodeOf(err) != v1.CodeReleaseConflict {
		t.Fatalf("want RELEASE_CONFLICT, got %v", err)
	}
	// 原来那份内容不能被换掉：制品不可变，复用一个目录等于允许「同一个版本号下换内容」。
	content, err := os.ReadFile(filepath.Join(releaseDir(t, adapter, spec, "rel_1"), "server"))
	if err != nil {
		t.Fatalf("读取: %v", err)
	}
	if string(content) != "first" {
		t.Fatalf("已存在的 release 内容被改写了: %q", content)
	}
}

func TestMaterializeFailureLeavesNoReleaseDir(t *testing.T) {
	adapter, _ := newTestAdapter(t)
	spec := goSpec(domain.SpecUnpack{Strategy: domain.UnpackTarGz}, "")

	// 声称是 tar-gz，实际给一段不是 gzip 的字节。
	err := adapter.Materialize(context.Background(), spec, "rel_1", strings.NewReader("not-a-gzip"))
	if err == nil {
		t.Fatal("不是 gzip 的字节必须让解包失败")
	}
	dir := releaseDir(t, adapter, spec, "rel_1")
	if _, statErr := os.Lstat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("解包失败后不该留下 release 目录（%s）", dir)
	}
	// 暂存目录也要清掉，否则下次同 ID 的部署会踩到残片。
	entries, err := os.ReadDir(filepath.Dir(dir))
	if err != nil {
		t.Fatalf("读取 releases 根: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "staging") {
			t.Fatalf("失败后留下了暂存目录 %q", entry.Name())
		}
	}
}

// ==== tar ====

func TestMaterializeTarWithStripComponents(t *testing.T) {
	adapter, _ := newTestAdapter(t)
	spec := goSpec(domain.SpecUnpack{Strategy: domain.UnpackTar, StripComponents: 1}, "")

	archive := buildTar(t,
		tarEntry{name: "release/", typeflag: tar.TypeDir, mode: 0o755},
		tarEntry{name: "release/bin/", typeflag: tar.TypeDir, mode: 0o750},
		tarEntry{name: "release/bin/server", typeflag: tar.TypeReg, mode: 0o755, body: "#!/bin/sh\n"},
		tarEntry{name: "release/README", typeflag: tar.TypeReg, mode: 0o644, body: "hello"},
	)
	if err := adapter.Materialize(context.Background(), spec, "rel_1", bytes.NewReader(archive)); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	dir := releaseDir(t, adapter, spec, "rel_1")
	// stripComponents=1 把顶层 `release/` 剥掉。
	if _, err := os.Stat(filepath.Join(dir, "bin", "server")); err != nil {
		t.Fatalf("剥掉一层之后应当有 bin/server: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(dir, "README"))
	if err != nil {
		t.Fatalf("读取 README: %v", err)
	}
	if string(content) != "hello" {
		t.Fatalf("README 内容不对: %q", content)
	}
	if _, err := os.Stat(filepath.Join(dir, "release")); !os.IsNotExist(err) {
		t.Fatal("顶层目录应当已被剥掉")
	}
	// 目录模式按归档。
	info, err := os.Stat(filepath.Join(dir, "bin"))
	if err != nil {
		t.Fatalf("stat bin: %v", err)
	}
	if info.Mode().Perm() != 0o750 {
		t.Fatalf("目录模式 want 0750, got %04o", info.Mode().Perm())
	}
}

func TestMaterializeRejectsAbsoluteAndDotDotEntries(t *testing.T) {
	cases := []struct {
		name  string
		entry string
	}{
		{"绝对路径条目", "/etc/passwd"},
		{"相对逃逸条目", "../outside/pwned"},
		{"夹在中间的逃逸", "bin/../../outside/pwned"},
		{"反斜杠条目", `bin\..\..\outside\pwned`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter, root := newTestAdapter(t)
			spec := goSpec(domain.SpecUnpack{Strategy: domain.UnpackTar}, "")

			// 哨兵：解析后可能被写到的地方。测试根前缀的**上一级**，任何逃逸都会落到这里。
			canary := filepath.Join(filepath.Dir(root), "outside", "pwned")
			_ = os.RemoveAll(filepath.Dir(canary))

			archive := buildTar(t, tarEntry{name: tc.entry, typeflag: tar.TypeReg, body: "x"})
			err := adapter.Materialize(context.Background(), spec, "rel_1", bytes.NewReader(archive))
			if err == nil {
				t.Fatalf("条目 %q 必须被拒", tc.entry)
			}
			if domain.CodeOf(err) != v1.CodeArtifactUnpackFailed {
				t.Fatalf("want ARTIFACT_UNPACK_FAILED, got %v", err)
			}
			if _, statErr := os.Stat(canary); !os.IsNotExist(statErr) {
				t.Fatalf("哨兵路径被写到了：%s", canary)
			}
		})
	}
}

// 这条是 tar slip 的经典形态：先放一个指向目录之外的符号链接，再往链接里写文件。
// 「返回了错误」不足以说明问题——必须确认**外面那个文件没被创建**。
func TestMaterializeRejectsSymlinkTraversal(t *testing.T) {
	adapter, root := newTestAdapter(t)
	spec := goSpec(domain.SpecUnpack{Strategy: domain.UnpackTar}, "")

	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	canary := filepath.Join(outside, "pwned")

	archive := buildTar(t,
		tarEntry{name: "escape", typeflag: tar.TypeSymlink, linkname: outside},
		tarEntry{name: "escape/pwned", typeflag: tar.TypeReg, body: "owned"},
	)
	err := adapter.Materialize(context.Background(), spec, "rel_1", bytes.NewReader(archive))
	if err == nil {
		t.Fatal("穿过符号链接写文件必须被拒")
	}
	if _, statErr := os.Stat(canary); !os.IsNotExist(statErr) {
		t.Fatalf("哨兵文件被写出来了：%s", canary)
	}

	// 同一个道理的另一半：先放普通文件 `escape`，再放 `escape/pwned` —— 父路径不是目录，
	// 同样不能穿过去。
	archive = buildTar(t,
		tarEntry{name: "escape", typeflag: tar.TypeReg, body: "file"},
		tarEntry{name: "escape/pwned", typeflag: tar.TypeReg, body: "owned"},
	)
	if err := adapter.Materialize(context.Background(), spec, "rel_2", bytes.NewReader(archive)); err == nil {
		t.Fatal("父路径不是目录时必须被拒")
	}
}

// 先放符号链接、再放同名普通文件：O_CREATE 会跟随链接，把内容写到链接指向的地方。
func TestMaterializeRejectsFileOverSymlink(t *testing.T) {
	adapter, root := newTestAdapter(t)
	spec := goSpec(domain.SpecUnpack{Strategy: domain.UnpackTar}, "")

	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	canary := filepath.Join(outside, "pwned")

	archive := buildTar(t,
		tarEntry{name: "pwned", typeflag: tar.TypeSymlink, linkname: canary},
		tarEntry{name: "pwned", typeflag: tar.TypeReg, body: "owned"},
	)
	if err := adapter.Materialize(context.Background(), spec, "rel_1", bytes.NewReader(archive)); err == nil {
		t.Fatal("在同名符号链接上写文件必须被拒")
	}
	if _, statErr := os.Stat(canary); !os.IsNotExist(statErr) {
		t.Fatalf("哨兵文件被写出来了：%s", canary)
	}
}

// 符号链接的目标允许指向外面（共享库、/etc 下的配置是正常用法）：它只是一个名字。
func TestMaterializeAllowsSymlinkPointingOutside(t *testing.T) {
	adapter, _ := newTestAdapter(t)
	spec := goSpec(domain.SpecUnpack{Strategy: domain.UnpackTar}, "")

	archive := buildTar(t,
		tarEntry{name: "config", typeflag: tar.TypeSymlink, linkname: "/etc/orders/config.yaml"},
	)
	if err := adapter.Materialize(context.Background(), spec, "rel_1", bytes.NewReader(archive)); err != nil {
		t.Fatalf("指向外部的符号链接本身应当被允许: %v", err)
	}
	target, err := os.Readlink(filepath.Join(releaseDir(t, adapter, spec, "rel_1"), "config"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != "/etc/orders/config.yaml" {
		t.Fatalf("链接目标不对: %q", target)
	}
}

func TestMaterializeHardlinkMustStayInside(t *testing.T) {
	adapter, _ := newTestAdapter(t)
	spec := goSpec(domain.SpecUnpack{Strategy: domain.UnpackTar}, "")

	// 指向 release 之外的硬链接：拒绝。
	archive := buildTar(t, tarEntry{name: "leak", typeflag: tar.TypeLink, linkname: "../../../etc/passwd"})
	err := adapter.Materialize(context.Background(), spec, "rel_1", bytes.NewReader(archive))
	if err == nil {
		t.Fatal("指向 release 之外的硬链接必须被拒")
	}
	if domain.CodeOf(err) != v1.CodeArtifactUnpackFailed {
		t.Fatalf("want ARTIFACT_UNPACK_FAILED, got %v", err)
	}

	// 指向 release 之内的硬链接：允许，且是同一个 inode。
	archive = buildTar(t,
		tarEntry{name: "server", typeflag: tar.TypeReg, body: "binary"},
		tarEntry{name: "server-link", typeflag: tar.TypeLink, linkname: "server"},
	)
	if err := adapter.Materialize(context.Background(), spec, "rel_2", bytes.NewReader(archive)); err != nil {
		t.Fatalf("指向 release 内部的硬链接应当被允许: %v", err)
	}
	dir := releaseDir(t, adapter, spec, "rel_2")
	original, err := os.Stat(filepath.Join(dir, "server"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	linked, err := os.Stat(filepath.Join(dir, "server-link"))
	if err != nil {
		t.Fatalf("stat link: %v", err)
	}
	if !os.SameFile(original, linked) {
		t.Fatal("硬链接应当与目标指向同一个 inode")
	}
}

// ==== zip ====

func TestMaterializeZip(t *testing.T) {
	adapter, _ := newTestAdapter(t)
	spec := goSpec(domain.SpecUnpack{Strategy: domain.UnpackZip, StripComponents: 1}, "")

	archive := buildZip(t,
		zipEntry{name: "dist/", mode: os.ModeDir | 0o755},
		zipEntry{name: "dist/app.jar", body: "jar-bytes", mode: 0o644},
		zipEntry{name: "dist/lib/nested.txt", body: "nested", mode: 0o644},
	)
	if err := adapter.Materialize(context.Background(), spec, "rel_1", bytes.NewReader(archive)); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	dir := releaseDir(t, adapter, spec, "rel_1")
	content, err := os.ReadFile(filepath.Join(dir, "app.jar"))
	if err != nil {
		t.Fatalf("读取 app.jar: %v", err)
	}
	if string(content) != "jar-bytes" {
		t.Fatalf("内容不对: %q", content)
	}
	if _, err := os.Stat(filepath.Join(dir, "lib", "nested.txt")); err != nil {
		t.Fatalf("嵌套条目应当被解出来: %v", err)
	}
}

func TestMaterializeZipRejectsTraversal(t *testing.T) {
	adapter, root := newTestAdapter(t)
	spec := goSpec(domain.SpecUnpack{Strategy: domain.UnpackZip}, "")

	canary := filepath.Join(filepath.Dir(root), "outside", "pwned")

	archive := buildZip(t, zipEntry{name: "../outside/pwned", body: "x", mode: 0o644})
	err := adapter.Materialize(context.Background(), spec, "rel_1", bytes.NewReader(archive))
	if err == nil {
		t.Fatal("zip 的逃逸条目必须被拒")
	}
	if _, statErr := os.Stat(canary); !os.IsNotExist(statErr) {
		t.Fatalf("哨兵路径被写到了：%s", canary)
	}
}

// ==== current 指针 ====

func TestActivateSwitchesCurrentAtomically(t *testing.T) {
	adapter, _ := newTestAdapter(t)
	spec := goSpec(domain.SpecUnpack{Strategy: domain.UnpackNone}, "server")

	for _, id := range []string{"rel_1", "rel_2"} {
		if err := adapter.Materialize(context.Background(), spec, id, strings.NewReader(id)); err != nil {
			t.Fatalf("Materialize %s: %v", id, err)
		}
	}

	if err := adapter.Activate(context.Background(), spec, "", "rel_1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	current := filepath.Join(adapter.RootPath(domain.ReleaseRootDir(spec.Application)), "current")
	target, err := os.Readlink(current)
	if err != nil {
		t.Fatalf("readlink current: %v", err)
	}
	// 目标写成**相对名字**：整个 releases 根可以被整体搬走或换前缀。
	if target != "rel_1" {
		t.Fatalf("current 应当指向相对名字 rel_1，got %q", target)
	}

	if err := adapter.Activate(context.Background(), spec, "", "rel_2"); err != nil {
		t.Fatalf("第二次 Activate: %v", err)
	}
	target, err = os.Readlink(current)
	if err != nil {
		t.Fatalf("readlink current: %v", err)
	}
	if target != "rel_2" {
		t.Fatalf("current 应当被换到 rel_2，got %q", target)
	}
	// 切换不该留下 .tmp。
	if _, err := os.Lstat(current + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("切换之后不该留下 current.tmp")
	}

	current_, err := adapter.Current(spec)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if current_ != "rel_2" {
		t.Fatalf("Current want rel_2, got %q", current_)
	}
}

func TestActivateRejectsMissingRelease(t *testing.T) {
	adapter, _ := newTestAdapter(t)
	spec := goSpec(domain.SpecUnpack{Strategy: domain.UnpackNone}, "server")

	err := adapter.Activate(context.Background(), spec, "", "rel_missing")
	if domain.CodeOf(err) != v1.CodeReleaseNotFound {
		t.Fatalf("want RELEASE_NOT_FOUND, got %v", err)
	}
}

// ==== 删除与列举 ====

func TestRemoveRefusesCurrentAndIsIdempotent(t *testing.T) {
	adapter, _ := newTestAdapter(t)
	spec := goSpec(domain.SpecUnpack{Strategy: domain.UnpackNone}, "server")

	for _, id := range []string{"rel_1", "rel_2"} {
		if err := adapter.Materialize(context.Background(), spec, id, strings.NewReader(id)); err != nil {
			t.Fatalf("Materialize %s: %v", id, err)
		}
	}
	if err := adapter.Activate(context.Background(), spec, "", "rel_1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	// 当前激活的那份删不得：删掉它，服务下次重启就起不来。
	err := adapter.Remove(context.Background(), spec, "rel_1")
	if domain.CodeOf(err) != v1.CodeReleaseConflict {
		t.Fatalf("want RELEASE_CONFLICT, got %v", err)
	}
	if _, statErr := os.Stat(releaseDir(t, adapter, spec, "rel_1")); statErr != nil {
		t.Fatalf("被拒的删除不该真的删掉东西: %v", statErr)
	}

	// 非 current 的可以删，而且删两次都成功（清理路径会被重试）。
	if err := adapter.Remove(context.Background(), spec, "rel_2"); err != nil {
		t.Fatalf("Remove rel_2: %v", err)
	}
	if err := adapter.Remove(context.Background(), spec, "rel_2"); err != nil {
		t.Fatalf("重复 Remove 必须成功（幂等）: %v", err)
	}
}

func TestListSkipsCurrentAndStaging(t *testing.T) {
	adapter, _ := newTestAdapter(t)
	spec := goSpec(domain.SpecUnpack{Strategy: domain.UnpackNone}, "server")

	for _, id := range []string{"rel_1", "rel_2"} {
		if err := adapter.Materialize(context.Background(), spec, id, strings.NewReader(id)); err != nil {
			t.Fatalf("Materialize %s: %v", id, err)
		}
	}
	if err := adapter.Activate(context.Background(), spec, "", "rel_1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	// 手工造一个暂存目录：它不该出现在「有哪些 release」的答案里。
	staging := filepath.Join(adapter.RootPath(domain.ReleaseRootDir(spec.Application)), ".rel_9.staging")
	if err := os.MkdirAll(staging, 0o750); err != nil {
		t.Fatalf("mkdir staging: %v", err)
	}

	releases, err := adapter.List(context.Background(), spec)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(releases) != 2 || releases[0] != "rel_1" || releases[1] != "rel_2" {
		t.Fatalf("List 结果不对: %v", releases)
	}
}

func TestMaterializeRejectsBadReleaseID(t *testing.T) {
	adapter, _ := newTestAdapter(t)
	spec := goSpec(domain.SpecUnpack{Strategy: domain.UnpackNone}, "server")

	for _, id := range []string{"../escape", "a/b", "", ".hidden", "-flag"} {
		err := adapter.Materialize(context.Background(), spec, id, strings.NewReader("x"))
		if err == nil {
			t.Fatalf("release ID %q 必须被拒（它会直接进路径）", id)
		}
	}
}

// Materialize 之后没有可读的句柄残留：读一遍刚解出来的文件，确认内容完整。
func TestMaterializeLeavesReadableContent(t *testing.T) {
	adapter, _ := newTestAdapter(t)
	spec := goSpec(domain.SpecUnpack{Strategy: domain.UnpackTar}, "")

	archive := buildTar(t, tarEntry{name: "data.txt", typeflag: tar.TypeReg, body: strings.Repeat("x", 1024)})
	if err := adapter.Materialize(context.Background(), spec, "rel_1", bytes.NewReader(archive)); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	file, err := os.Open(filepath.Join(releaseDir(t, adapter, spec, "rel_1"), "data.txt"))
	if err != nil {
		t.Fatalf("打开: %v", err)
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil {
		t.Fatalf("读取: %v", err)
	}
	if len(content) != 1024 {
		t.Fatalf("内容长度不对: %d", len(content))
	}
}

// ==== 槽位（迭代 4）====

// 两个槽位各有自己的 current 指针：这正是「两个版本同时运行」的物质基础。
func TestActivateSlotKeepsPointersIndependent(t *testing.T) {
	adapter, root := newTestAdapter(t)
	spec := goSpec(domain.SpecUnpack{Strategy: domain.UnpackNone}, "server")
	// 蓝绿应用：两个槽位各声明一个端口。
	spec.Exec.Slots = map[domain.Slot]domain.SpecSlot{
		domain.SlotBlue: {Ports: []int{18081},
			Readiness: domain.SpecReadiness{Type: domain.ReadinessTCP, Target: "127.0.0.1:18081"}},
		domain.SlotGreen: {Ports: []int{18082},
			Readiness: domain.SpecReadiness{Type: domain.ReadinessTCP, Target: "127.0.0.1:18082"}},
	}
	spec.Exec.Ports = nil
	spec.Health.Readiness = domain.SpecReadiness{}
	spec.Nginx = domain.SpecNginx{Listen: 8080}
	spec.Systemd.UnitName = ""

	for _, id := range []string{"rel_1", "rel_2"} {
		if err := adapter.Materialize(context.Background(), spec, id, strings.NewReader(id)); err != nil {
			t.Fatalf("Materialize %s: %v", id, err)
		}
	}
	if err := adapter.Activate(context.Background(), spec, domain.SlotBlue, "rel_1"); err != nil {
		t.Fatalf("Activate blue: %v", err)
	}
	if err := adapter.Activate(context.Background(), spec, domain.SlotGreen, "rel_2"); err != nil {
		t.Fatalf("Activate green: %v", err)
	}

	blue := filepath.Join(root, domain.SlotCurrentDir(spec.Application, domain.SlotBlue))
	green := filepath.Join(root, domain.SlotCurrentDir(spec.Application, domain.SlotGreen))

	// 目标写成**相对路径**：从 <app>/slots/<slot>/current 上跳两级回到 <app>，再进 releases。
	// 相对而不是绝对，是为了让整个目录树可以被整体搬走（与单槽 current 同一条理由）。
	blueTarget, err := os.Readlink(blue)
	if err != nil {
		t.Fatalf("readlink blue: %v", err)
	}
	if want := filepath.Join("..", "..", "releases", "rel_1"); blueTarget != want {
		t.Fatalf("blue 的目标 want %q, got %q", want, blueTarget)
	}
	greenTarget, err := os.Readlink(green)
	if err != nil {
		t.Fatalf("readlink green: %v", err)
	}
	if want := filepath.Join("..", "..", "releases", "rel_2"); greenTarget != want {
		t.Fatalf("green 的目标 want %q, got %q", want, greenTarget)
	}

	// 两个指针解析到的目录必须存在，且是各自的 release。
	for slot, want := range map[domain.Slot]string{domain.SlotBlue: "rel_1", domain.SlotGreen: "rel_2"} {
		resolved, err := filepath.EvalSymlinks(filepath.Join(root, domain.SlotCurrentDir(spec.Application, slot)))
		if err != nil {
			t.Fatalf("解析 %s 的 current: %v", slot, err)
		}
		// 期望值也过一遍 EvalSymlinks：macOS 上 /var 是指向 /private/var 的符号链接，
		// 只解析一边会让两个「同一个目录」的写法比不相等。
		expected, err := filepath.EvalSymlinks(releaseDir(t, adapter, spec, want))
		if err != nil {
			t.Fatalf("解析期望目录: %v", err)
		}
		if resolved != expected {
			t.Fatalf("%s 的 current 应当指向 %s，got %s", slot, want, resolved)
		}
	}

	// 单槽那个 current 不该被建出来：蓝绿应用没有「一个 current」这回事，
	// 建出来只会让「现在跑的是哪个版本」多一个错误的答案。
	if _, err := os.Lstat(filepath.Join(root, domain.ReleaseRootDir(spec.Application), "current")); !os.IsNotExist(err) {
		t.Fatal("蓝绿应用的 Activate 不该建出单槽的 current 指针")
	}

	// CurrentIn 按槽位回答「这一侧跑的是哪个版本」。
	for slot, want := range map[domain.Slot]string{domain.SlotBlue: "rel_1", domain.SlotGreen: "rel_2"} {
		got, err := adapter.CurrentIn(spec, slot)
		if err != nil || got != want {
			t.Fatalf("CurrentIn(%s) want %s, got %q err=%v", slot, want, got, err)
		}
	}
}

// 保留策略的「永不删正在服务的目录」这条保底要按**槽位**生效：删掉任何一侧正指向的目录，
// 那一侧的下一次重启就会起不来，而另一侧还好好跑着——故障看起来会很随机。
func TestRemoveRefusesReleaseUsedByAnySlot(t *testing.T) {
	adapter, root := newTestAdapter(t)
	spec := goSpec(domain.SpecUnpack{Strategy: domain.UnpackNone}, "server")
	spec.Exec.Slots = map[domain.Slot]domain.SpecSlot{
		domain.SlotBlue: {Ports: []int{18081},
			Readiness: domain.SpecReadiness{Type: domain.ReadinessTCP, Target: "127.0.0.1:18081"}},
		domain.SlotGreen: {Ports: []int{18082},
			Readiness: domain.SpecReadiness{Type: domain.ReadinessTCP, Target: "127.0.0.1:18082"}},
	}
	spec.Exec.Ports = nil
	spec.Health.Readiness = domain.SpecReadiness{}
	spec.Nginx = domain.SpecNginx{Listen: 8080}
	spec.Systemd.UnitName = ""

	for _, id := range []string{"rel_1", "rel_2"} {
		if err := adapter.Materialize(context.Background(), spec, id, strings.NewReader(id)); err != nil {
			t.Fatalf("Materialize %s: %v", id, err)
		}
	}
	if err := adapter.Activate(context.Background(), spec, domain.SlotBlue, "rel_1"); err != nil {
		t.Fatalf("Activate blue: %v", err)
	}
	if err := adapter.Activate(context.Background(), spec, domain.SlotGreen, "rel_2"); err != nil {
		t.Fatalf("Activate green: %v", err)
	}

	// 两侧都不许删，而且报错要说清是哪一侧在用。
	for _, id := range []string{"rel_1", "rel_2"} {
		err := adapter.Remove(context.Background(), spec, id)
		if domain.CodeOf(err) != v1.CodeReleaseConflict {
			t.Fatalf("删除 %s want RELEASE_CONFLICT, got %v", id, err)
		}
		if !strings.Contains(err.Error(), "槽位") {
			t.Fatalf("报错要说清是哪一侧在用：%v", err)
		}
		if _, statErr := os.Stat(releaseDir(t, adapter, spec, id)); statErr != nil {
			t.Fatalf("被拒的删除不该真的删掉东西（%s）: %v", id, statErr)
		}
	}
	_ = root
}
