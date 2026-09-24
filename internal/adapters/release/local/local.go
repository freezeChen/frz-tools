// Package local 是 ReleaseAdapter 的实现：把一份制品解到 release 目录，并管理版本的
// 切换与清理。
//
// 它是**唯一**会把外部字节变成可执行文件的地方，因此这里的每一条校验都不是格式偏好：
// 解包错了，运维会得到一个「解出来了、但内容不是制品」的 release 目录；而路径穿越错了，
// 后果是往 release 目录之外写文件。两者都不会在部署时报错。
package local

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
	"github.com/freezeChen/frz-tools/internal/sysuser"
)

// releaseDirMode 是 release 目录的模式（1c 第 15 节决定 4）：0750，属主 runUser。
const releaseDirMode os.FileMode = 0o750

// 单文件制品（unpack.strategy=none）落盘时的模式：它就是要被执行或由解释器读取的那个
// 文件，0644 会让「解出来了却起不来」变成一个奇怪的错误。
const singleFileMode os.FileMode = 0o755

// currentLink 是 current 指针的名字（规格 D3）。
const currentLink = "current"

// releaseIDPattern 是 release ID 的字符集。它是**安全边界**：releaseID 会直接进路径，
// 放开字符集就等于放开路径穿越（`../` 是合法字符串）。
var releaseIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Adapter 实现 application.ReleaseAdapter。
type Adapter struct {
	// root 是路径前缀重定向，生产传 "/"。这样无特权环境（开发机、容器）也能在临时目录里
	// 跑完整断言，且断言的是真实的文件与模式——与 systemd / files 适配器同一个理由。
	root  string
	owner sysuser.Resolver
}

// Option 用于在装配与测试里改参数。
type Option func(*Adapter)

// WithRoot 覆盖路径前缀（测试用；生产用 "/"）。
func WithRoot(root string) Option {
	return func(a *Adapter) { a.root = root }
}

// WithOwnerResolver 替换属主解析：测试进程通常不是 root，无法把文件 chown 给别的用户，
// 因此注入当前进程的 uid/gid，从而仍能断言「chown 真的被调用过」。
func WithOwnerResolver(resolver sysuser.Resolver) Option {
	return func(a *Adapter) { a.owner = resolver }
}

func New(opts ...Option) *Adapter {
	adapter := &Adapter{root: "/", owner: sysuser.OSLookup}
	for _, opt := range opts {
		opt(adapter)
	}
	if adapter.root == "" {
		adapter.root = "/"
	}
	return adapter
}

// RootPath 把约定的绝对路径映射到本适配器的实际前缀之下。导出它是为了让测试断言真实
// 产物，而不是复制一份前缀规则——复制出来的规则迟早会与实现分叉。
func (a *Adapter) RootPath(absolute string) string {
	return filepath.Join(a.root, filepath.FromSlash(absolute))
}

// Materialize 把制品字节解到该 release 的目录里。
//
// 两个刻意的做法：
//
//   - **解到临时目录再改名**（`.staging` → 最终名字）。解到一半失败时，最终目录根本不会
//     出现；否则会留下一个「看起来完整、其实缺半截」的 release 目录，而它一旦被切上去
//     就是一次静默的坏发布。
//   - **已存在的目录直接拒绝**（`RELEASE_CONFLICT`）。制品不可变，复用一个目录等于允许
//     「同一个版本号下换了内容」——那是最难查的一类漂移。
func (a *Adapter) Materialize(ctx context.Context, spec *domain.ApplicationSpec, releaseID string, artifact io.Reader) error {
	if spec == nil {
		return domain.NewError(v1.CodeInvalidRequest, "release 适配器需要非空的应用规格")
	}
	// 不假设调用方校验过规格：未经验证的规格被直接解包，是最危险的一类误用。
	if err := spec.Validate(); err != nil {
		return err
	}
	if !releaseIDPattern.MatchString(releaseID) {
		return domain.NewError(v1.CodeInvalidRequest, "release ID 取值非法: %q", releaseID)
	}
	if artifact == nil {
		return domain.NewError(v1.CodeInvalidRequest, "缺少制品内容")
	}

	owner, err := a.ownership(spec.Exec.RunUser)
	if err != nil {
		return err
	}
	root := a.releaseRoot(spec)
	if err := a.ensureRoot(root, owner); err != nil {
		return err
	}

	final := filepath.Join(root, releaseID)
	if _, err := os.Lstat(final); err == nil {
		return domain.NewError(v1.CodeReleaseConflict, "release 目录 %q 已存在", final)
	} else if !os.IsNotExist(err) {
		return domain.NewError(v1.CodeInternal, "检查 release 目录 %q 失败: %v", final, err)
	}

	staging := filepath.Join(root, "."+releaseID+".staging")
	// 上一次崩溃可能留下同名的暂存目录；它是我们自己的中间产物，直接清掉重来。
	if err := os.RemoveAll(staging); err != nil {
		return domain.NewError(v1.CodeInternal, "清理暂存目录 %q 失败: %v", staging, err)
	}
	if err := os.Mkdir(staging, releaseDirMode); err != nil {
		return domain.NewError(v1.CodeInternal, "创建暂存目录 %q 失败: %v", staging, err)
	}

	if err := unpackArtifact(ctx, spec, artifact, staging, owner); err != nil {
		_ = os.RemoveAll(staging)
		return err
	}
	if err := os.Chmod(staging, releaseDirMode); err != nil {
		_ = os.RemoveAll(staging)
		return domain.NewError(v1.CodeInternal, "收敛 %q 权限失败: %v", staging, err)
	}
	if err := os.Chown(staging, owner.uid, owner.gid); err != nil {
		_ = os.RemoveAll(staging)
		return domain.NewError(v1.CodeInternal, "设置 %q 属主失败: %v", staging, err)
	}
	if err := os.Rename(staging, final); err != nil {
		_ = os.RemoveAll(staging)
		return domain.NewError(v1.CodeInternal, "把暂存目录改名为 %q 失败: %v", final, err)
	}
	return nil
}

// Activate 把 current 指针切到该 release。
//
// 切换是**一次原子 rename**：先建 `current.tmp` 再改名覆盖。运维可能在切换的任意瞬间
// 重启 systemd，而「读到一个半成品链接」会让服务起在一个不存在的目录上。
func (a *Adapter) Activate(ctx context.Context, spec *domain.ApplicationSpec, releaseID string) error {
	if spec == nil {
		return domain.NewError(v1.CodeInvalidRequest, "release 适配器需要非空的应用规格")
	}
	if err := ctx.Err(); err != nil {
		return domain.NewError(v1.CodeExecCancelled, "切换被取消")
	}
	if !releaseIDPattern.MatchString(releaseID) {
		return domain.NewError(v1.CodeInvalidRequest, "release ID 取值非法: %q", releaseID)
	}

	root := a.releaseRoot(spec)
	target := filepath.Join(root, releaseID)
	info, err := os.Stat(target)
	if err != nil || !info.IsDir() {
		return domain.NewError(v1.CodeReleaseNotFound, "release 目录 %q 不存在", target)
	}

	link := filepath.Join(root, currentLink)
	tmp := link + ".tmp"
	// 符号链接的目标写**相对名字**（`<releaseID>`）而不是绝对路径：整个 releases 根可以
	// 被整体搬走或换前缀（测试里的 RootPath 就是这么做的），绝对路径会在那时失效。
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return domain.NewError(v1.CodeInternal, "清理 %q 失败: %v", tmp, err)
	}
	if err := os.Symlink(releaseID, tmp); err != nil {
		return domain.NewError(v1.CodeInternal, "创建 %q 失败: %v", tmp, err)
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return domain.NewError(v1.CodeInternal, "把 %q 改名成 %q 失败: %v", tmp, link, err)
	}
	return nil
}

// Remove 删掉一个 release 目录。
//
// 它**拒绝删除 current 指向的那个**：那是正在跑的版本，删掉它等于让服务在下一次重启时
// 起不来。保留策略的「永不删 current」这条保底就落在这里——调用方可以算错，但这个动作
// 不会被执行。
func (a *Adapter) Remove(ctx context.Context, spec *domain.ApplicationSpec, releaseID string) error {
	if spec == nil {
		return domain.NewError(v1.CodeInvalidRequest, "release 适配器需要非空的应用规格")
	}
	if err := ctx.Err(); err != nil {
		return domain.NewError(v1.CodeExecCancelled, "清理被取消")
	}
	if !releaseIDPattern.MatchString(releaseID) {
		return domain.NewError(v1.CodeInvalidRequest, "release ID 取值非法: %q", releaseID)
	}

	root := a.releaseRoot(spec)
	if current, ok := a.readCurrent(root); ok && current == releaseID {
		return domain.NewError(v1.CodeReleaseConflict,
			"%s 是当前激活的 release，不能删除", releaseID)
	}
	target := filepath.Join(root, releaseID)
	if _, err := os.Lstat(target); err != nil {
		if os.IsNotExist(err) {
			// 幂等：已经不在了就是成功。清理路径会被重试。
			return nil
		}
		return domain.NewError(v1.CodeInternal, "检查 %q 失败: %v", target, err)
	}
	if err := os.RemoveAll(target); err != nil {
		return domain.NewError(v1.CodeInternal, "删除 %q 失败: %v", target, err)
	}
	return nil
}

// List 列出磁盘上实际存在的 release 目录（跳过 current 指针与暂存目录）。
//
// 它回答的是「盘上有什么」，而不是「库里记了什么」——两者不一致时，这里给出的是事实。
func (a *Adapter) List(ctx context.Context, spec *domain.ApplicationSpec) ([]string, error) {
	if spec == nil {
		return nil, domain.NewError(v1.CodeInvalidRequest, "release 适配器需要非空的应用规格")
	}
	if err := ctx.Err(); err != nil {
		return nil, domain.NewError(v1.CodeExecCancelled, "列举被取消")
	}

	root := a.releaseRoot(spec)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, domain.NewError(v1.CodeInternal, "读取 %q 失败: %v", root, err)
	}

	var releases []string
	for _, entry := range entries {
		name := entry.Name()
		if name == currentLink || strings.HasPrefix(name, ".") {
			continue
		}
		if !entry.IsDir() {
			continue
		}
		releases = append(releases, name)
	}
	return releases, nil
}

// Current 返回 current 指向的 release ID；没有 current 时返回空串。
//
// 它不在端口上（端口只有物化/切换/删除/列举四件事），但部署流程需要它来判断
// 「上一次成功的是哪个版本」——回滚目标就是从这里读出来的。
func (a *Adapter) Current(spec *domain.ApplicationSpec) (string, error) {
	if spec == nil {
		return "", domain.NewError(v1.CodeInvalidRequest, "release 适配器需要非空的应用规格")
	}
	current, _ := a.readCurrent(a.releaseRoot(spec))
	return current, nil
}

// readCurrent 读 current 指针。它刻意**不跟随**链接去校验目标存在：那是调用方的判断，
// 这里只回答「指针指着谁」。
func (a *Adapter) readCurrent(root string) (string, bool) {
	target, err := os.Readlink(filepath.Join(root, currentLink))
	if err != nil {
		return "", false
	}
	return filepath.Base(target), true
}

// releaseRoot 返回某个应用的 releases 根（已按 root 前缀重定向）。
func (a *Adapter) releaseRoot(spec *domain.ApplicationSpec) string {
	return a.RootPath(domain.ReleaseRootDir(spec.Application))
}

func (a *Adapter) ownership(runUser string) (ownership, error) {
	uid, gid, err := a.owner(runUser)
	if err != nil {
		return ownership{}, err
	}
	return ownership{uid: uid, gid: gid}, nil
}

// ensureRoot 建出 releases 根并收敛它的模式与属主。
//
// 生产路径上它通常已经由 RuntimeAdapter 的 Prepare 建好了（同一个路径，同一个约定）；
// 这里再建一次是为了让「只物化、还没 Prepare」的调用序列也能工作，而不是靠调用顺序的
// 默契——默契会在某次重构里断掉，而断掉的表现是「部署失败，因为目录不存在」。
func (a *Adapter) ensureRoot(root string, owner ownership) error {
	if err := os.MkdirAll(root, releaseDirMode); err != nil {
		return domain.NewError(v1.CodeInternal, "创建 release 根目录 %q 失败: %v", root, err)
	}
	// MkdirAll 对已存在的目录不改权限，因此叶子必须显式收敛——否则上一次留下的 0755
	// 会一直「看起来是对的」。
	if err := os.Chmod(root, releaseDirMode); err != nil {
		return domain.NewError(v1.CodeInternal, "收敛 %q 权限失败: %v", root, err)
	}
	if err := os.Chown(root, owner.uid, owner.gid); err != nil {
		return domain.NewError(v1.CodeInternal, "设置 %q 属主失败: %v", root, err)
	}
	return nil
}

// ownership 是运行用户的 uid/gid。
type ownership struct {
	uid int
	gid int
}

// chown 会**跟随**符号链接（给链接的目标改属主），对符号链接本身要用 Lchown。
// 这两件事用错方向的后果是：解出来的符号链接指着的那个宿主文件被改了属主。
func (o ownership) chown(target string) error {
	err := os.Lchown(target, o.uid, o.gid)
	if err != nil {
		return domain.NewError(v1.CodeInternal, "设置 %q 属主失败: %v", target, err)
	}
	return nil
}

var errPathEscape = errors.New("条目名逃出了 release 目录")
