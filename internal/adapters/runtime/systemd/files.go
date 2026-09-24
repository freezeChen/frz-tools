package systemd

import (
	"bytes"
	"context"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/runtime/unitfile"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// ownership 是运行用户的 uid/gid，一次解析、多处使用，避免在每个调用点各查一次。
type ownership struct {
	uid int
	gid int
}

// resolvedSecrets 是一次 Prepare 中解析出的明文凭据。它刻意不实现 String：
// 明文不该因为某处顺手用了 %v 就出现在日志或审计里。
type resolvedSecrets struct {
	// env 是 kind=env 的凭据，值直接进 secrets.env。
	env map[string]string
	// file 是 kind=file 的凭据，内容进凭据文件，环境变量里传的是路径。
	file map[string]string
}

// ownerOf 解析运行用户。必须在 ensureUser 之后调用：用户还没建出来时这里必然失败。
func (a *Adapter) ownerOf(runUser string) (ownership, error) {
	uid, gid, err := a.owner(runUser)
	if err != nil {
		return ownership{}, err
	}
	return ownership{uid: uid, gid: gid}, nil
}

// ensureUser 保证运行用户存在：先用 id 探测，已存在时只校验、不改任何属性。
//
// 不直接跑 useradd 再容忍它的「已存在」退出码，原因有二：退出码因发行版而异；
// 而且后续迭代会有人手工调整过用户，把属性改回规格值等于回滚运维的调整。
func (a *Adapter) ensureUser(ctx context.Context, runUser, home string) error {
	result, err := a.runner(ctx, []string{idBin, "-u", runUser})
	if err != nil {
		return domain.NewError(v1.CodeInternal, "执行 %s 探测用户 %q 失败: %v", idBin, runUser, err)
	}
	if result.ExitCode == 0 {
		return nil
	}
	_, err = a.mustRun(ctx, v1.CodeInternal, useraddBin,
		"--system", "--no-create-home", "--home-dir", home, "--shell", nologinShell, runUser)
	return err
}

// ensureDirectories 建齐规格 §7 要求的目录：工作目录、日志目录、解包目录（0750）
// 与凭据目录（0700），属主都是运行用户。
//
// **releases 子树的内部不归它管**（domain.InReleaseTree）。那里的一切都由
// ReleaseAdapter 建：根由物化建、release 目录由解包建、`current` 由切换建。
// 这条分工不是洁癖——`exec.workingDirectory` 的推荐写法就是要写成 release 的
// `current`（那样 `java -jar app.jar` 才能按工作目录解析到制品里的 JAR），而
// Prepare 在**第一次部署**时跑在物化之前：它若「顺手」把 `current` 建成实体目录，
// 紧接着的 Activate 就会因为改名目标是目录而失败，报出来的还是一句与真实原因无关的
// 「改名失败」。根目录仍然要建：unit 的 ReadWritePaths= 指向它，而路径不存在会让
// systemd 的命名空间设置直接失败。
func (a *Adapter) ensureDirectories(spec *domain.ApplicationSpec, owner ownership) error {
	for _, dir := range []struct {
		path string
		mode os.FileMode
	}{
		{spec.Exec.WorkingDirectory, appDirMode},
		{spec.Logs.Directory, appDirMode},
		// 解包目录用 domain.ReleaseDir 的约定路径的父目录（releases 根），
		// 与 unit 的 ReadWritePaths 保持一致；release 目录本身属迭代 3。
		{domain.ReleaseRootDir(spec.Application), appDirMode},
		{domain.SecretsDir(spec.Application), secretDirMode},
	} {
		if domain.InReleaseTree(spec.Application, dir.path) {
			continue
		}
		if err := a.ensureDirectory(dir.path, dir.mode, owner); err != nil {
			return err
		}
	}
	return nil
}

// ensureDirectory 建目录并显式收敛叶子的权限与属主。
//
// MkdirAll 对已存在的目录不改权限：上一次留下的 0755 目录会一直「看起来是对的」，
// 直到真的有人去读它——凭据目录尤其不能这样。因此叶子必须显式 Chmod + Chown。
func (a *Adapter) ensureDirectory(absolute string, mode os.FileMode, owner ownership) error {
	target := a.RootPath(absolute)
	if err := a.ensureAncestors(filepath.Dir(target)); err != nil {
		return err
	}
	if err := os.MkdirAll(target, mode); err != nil {
		return domain.NewError(v1.CodeInternal, "创建目录 %q 失败: %v", target, err)
	}
	if err := os.Chmod(target, mode); err != nil {
		return domain.NewError(v1.CodeInternal, "收敛目录 %q 权限失败: %v", target, err)
	}
	if err := os.Chown(target, owner.uid, owner.gid); err != nil {
		return domain.NewError(v1.CodeInternal, "设置目录 %q 属主失败: %v", target, err)
	}
	return nil
}

// ensureAncestors 把 dir 之下缺失的中间目录补出来：它们是路径上的「过路目录」
// （/etc/opsd/apps、/opt/opsd/apps/<application>），不属于任何单个应用，
// 因此不改属主，也不动已经存在的目录。
//
// 为什么必须显式建、而且权限必须是 0751（可穿越、不可列目录）：运行用户即使拥有
// 叶子目录，也仍然需要每一级父目录的 +x 才能进去。MkdirAll 会用同一个 mode 建出所有
// 中间层（0700 的凭据目录会把 /etc/opsd/apps 也变成 0700），而且权限还会被 umask 削掉
// （umask 077 的机器上 0755 会变成 0700），运行用户于是永远进不了自己的目录。
// 这里显式 Chmod 就是为了不受 umask 影响。
func (a *Adapter) ensureAncestors(dir string) error {
	if dir == a.root || dir == string(filepath.Separator) || dir == "." || dir == "" {
		return nil
	}
	if _, err := os.Stat(dir); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return domain.NewError(v1.CodeInternal, "检查目录 %q 失败: %v", dir, err)
	}
	if err := a.ensureAncestors(filepath.Dir(dir)); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o751); err != nil && !os.IsExist(err) {
		return domain.NewError(v1.CodeInternal, "创建目录 %q 失败: %v", dir, err)
	}
	if err := os.Chmod(dir, 0o751); err != nil {
		return domain.NewError(v1.CodeInternal, "收敛目录 %q 权限失败: %v", dir, err)
	}
	return nil
}

// resolveSecrets 在使用时刻解析全部凭据。先全部解析、后统一落盘，
// 是为了让 SECRET_UNRESOLVED 出现在「还没有写任何文件」的时候。
func (a *Adapter) resolveSecrets(ctx context.Context, spec *domain.ApplicationSpec) (*resolvedSecrets, error) {
	if a.resolver == nil {
		return nil, domain.NewError(v1.CodeInternal,
			"systemd 适配器缺少 SecretResolver，无法把凭据交付给应用")
	}
	resolved := &resolvedSecrets{env: map[string]string{}, file: map[string]string{}}

	// 按名字排序解析（SecretEnvNames 已排序），保证错误与写入的顺序稳定可测。
	for _, name := range spec.SecretEnvNames() {
		ref := spec.Exec.SecretEnvironment[name]
		value, err := a.resolver.Resolve(ctx, ref)
		if err != nil {
			return nil, err
		}
		// EnvironmentFile 承载不了真实换行（续行会把换行吃掉），必须显式拒绝，
		// 而不是让进程收到一个被静默改写的凭据。
		if unitfile.HasNewline(value) {
			return nil, domain.NewError(v1.CodeSecretUnresolved,
				"secretEnvironment[%q] 的值含换行：EnvironmentFile 无法承载真实换行，必须声明为 kind: file（当前 kind=%s）",
				name, ref.Kind)
		}
		resolved.env[name] = value
	}

	for _, name := range spec.SecretFileNames() {
		value, err := a.resolver.Resolve(ctx, spec.Exec.SecretEnvironment[name])
		if err != nil {
			return nil, err
		}
		// kind=file 的多行值是允许的：它落成独立文件，不需要经过环境文件。
		resolved.file[name] = value
	}
	return resolved, nil
}

// ensureCredentialReachable 保证运行用户能真正打开 kind=file 的凭据文件。
//
// 为什么必需：凭据文件是**应用进程以 runUser 身份自己按路径**打开的，路径上任何一级
// 目录对 runUser 缺 +x，应用就会在运行时拿到 EACCES。而 1c §7 把落点定在
// /etc/opsd/apps/<application>.secrets/ 下，迭代 0 §9 的安装约定（test/linux/verify.sh
// 也断言 750）把 /etc/opsd 建成 0750、属主是 opsd 自己：0750 对 runUser 意味着 other 位
// 为 `---`，穿越被拒。kind=env 的环境文件不受影响（那是 systemd 以 root 读的），
// 所以这个缺口只在 kind=file 上暴露，而且只在真机上暴露——本地测试的根前缀是我们自己
// 以 0751 建的，正好绕过了它。
//
// 处理分两类：
//   - 属于我们的层级（属主就是 opsd 自己的 euid）：只补穿越位（mode|0111），不放开读与
//     列目录位——opsd 自己的 config.yaml(0600) 与 secrets/(0700) 的保护不变；
//   - 不属于我们的层级：不修改（那可能是 root 或运维的目录），改为校验 runUser 能否穿越，
//     不能就把 Prepare 顶掉并给出确切的修复命令。
//
// 宁可 Prepare 失败，也不要让应用带着一个打不开的凭据路径启动：那种失败发生在目标主机的
// 应用进程里，比这里难排查一个数量级。
func (a *Adapter) ensureCredentialReachable(spec *domain.ApplicationSpec, owner ownership) error {
	levels := credentialPathLevels(spec.Application)
	// levels 的末项是凭据目录本身：按 §7 它是 0700、属主 runUser，模式由 ensureDirectory
	// 收敛，这里只校验、不参与「补穿越位」——否则在属主恰好是 opsd 的环境里它会被放大成
	// 0711（测试环境正是这种情况），而一个「靠环境差异才不会发生」的行为不算行为。
	credentialDir := levels[len(levels)-1]

	// 归我们管理的只有这三级，它们由 opsd 创建（or 安装约定交给 opsd 的运行身份）。
	// 更高层（/、/etc）只校验、绝不修改：一次 Prepare 顺手把系统目录的权限位改宽，
	// 是那种「本地没人发现、真机上被安全扫描发现」的改动。
	managed := map[string]bool{
		credentialDir:                             true, // /etc/opsd/apps/<application>.secrets
		filepath.Dir(credentialDir):               true, // /etc/opsd/apps
		filepath.Dir(filepath.Dir(credentialDir)): true, // /etc/opsd
	}
	euid := a.euid()

	for _, absolute := range levels {
		target := a.RootPath(absolute)
		info, err := os.Stat(target)
		if err != nil {
			return domain.NewError(v1.CodeInternal, "检查凭据路径 %q 失败: %v", target, err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return domain.NewError(v1.CodeInternal, "无法读取 %q 的属主信息", target)
		}
		mode := info.Mode().Perm()
		dirUID, dirGID := int(stat.Uid), int(stat.Gid)

		if managed[absolute] && absolute != credentialDir && canChmod(euid, dirUID) && mode&0o111 != 0o111 {
			repaired := mode | 0o111
			if err := os.Chmod(target, repaired); err != nil {
				return domain.NewError(v1.CodePermissionDenied,
					"为运行用户 %s 补上对 %q 的穿越位失败: %v", spec.Exec.RunUser, target, err)
			}
			mode = repaired
		}
		if !traversable(mode, dirUID, dirGID, owner) {
			return domain.NewError(v1.CodePermissionDenied,
				"运行用户 %s 无法穿越 %q（模式 %04o，属主 %d:%d），应用将打不开自己的凭据文件；"+
					"修复：chmod %04o %s（只需放开穿越位，可以继续禁止读与列目录）",
				spec.Exec.RunUser, target, mode, dirUID, dirGID, mode|0o111, target)
		}
	}
	return nil
}

// credentialPathLevels 返回 kind=file 凭据路径上从根到叶的每一级目录，含凭据目录本身。
// 顺序是根到叶：出错时按阅读顺序报告，第一处不可穿越的层级就是需要修的那一级。
func credentialPathLevels(application string) []string {
	leaf := domain.SecretsDir(application)
	levels := []string{leaf}
	for dir := leaf; ; {
		parent := path.Dir(dir)
		if parent == dir || parent == "/" {
			break
		}
		levels = append(levels, parent)
		dir = parent
	}
	slices.Reverse(levels)
	return levels
}

// canChmod 报告本进程能否收敛某个目录的权限位：属主是自己，或者本进程是 root。
//
// 为什么必须有 root 这一支：安装约定把 /etc/opsd 的属主给了 opsd 的**服务用户**
// （test/linux/verify.sh 里是 frz-ops），而 RuntimeAdapter 需要 root 才能建用户、
// 装 unit、改属主——此时守护进程的 euid 是 0、那个目录的属主却是 frz-ops，
// 「属主等于自己」这一条会把本该能修好的目录判成不可修，于是 Prepare 在一个完全
// 正常的部署上失败（这是 Linux 容器实跑发现的第一处真问题）。
func canChmod(euid, dirUID int) bool {
	return euid == 0 || dirUID == euid
}

// traversable 判断 uid/gid 这对身份能否穿越一个目录：只需要 +x，读与列目录都不需要。
// 只按「属主 / 主组 / 其他」三类位判定：运行用户的附加组来自 /etc/group，本轮不解析它，
// 保守判定只会让 Prepare 更早报错，不会放过真正不可穿越的路径。
func traversable(mode os.FileMode, dirUID, dirGID int, user ownership) bool {
	switch {
	case dirUID == user.uid:
		return mode&0o100 != 0
	case dirGID == user.gid:
		return mode&0o010 != 0
	default:
		return mode&0o001 != 0
	}
}

// writeEnvFiles 写两个环境文件与所有 kind=file 的凭据文件。
//
// 这些文件所在层级的模式只有一处来源：过路目录由 ensureAncestors 建 0751、
// 叶子由 ensureDirectory 按规格收敛；kind=file 的凭据链另外过一遍
// ensureCredentialReachable（它可能要补穿越位）。这里不再出现任何"默认 0755"的假设——
// 之前那句注释就是错的：没有任何一条路径会产出 0755。
func (a *Adapter) writeEnvFiles(spec *domain.ApplicationSpec, secrets *resolvedSecrets, owner ownership) error {
	if err := a.writeFile(spec, domain.EnvFilePath(spec.Application),
		unitfile.RenderEnvFile(spec.Exec.Environment), secretFileMode, owner); err != nil {
		return err
	}

	// 敏感环境文件总是写：unit 里对应的 EnvironmentFile= 没有 `-` 前缀，
	// 文件缺失会让 unit 启动失败——「缺凭据就不要起来」是有意的设计。
	secretEnv := map[string]string{}
	for name, value := range secrets.env {
		secretEnv[name] = value
	}
	for _, name := range spec.SecretFileNames() {
		secretEnv[name] = domain.SecretFileVarValue(spec.Application, name)
	}
	if err := a.writeFile(spec, domain.SecretsEnvFilePath(spec.Application),
		unitfile.RenderEnvFile(secretEnv), secretFileMode, owner); err != nil {
		return err
	}

	for _, name := range spec.SecretFileNames() {
		if err := a.writeFile(spec, filepath.Join(domain.SecretsDir(spec.Application), name),
			[]byte(secrets.file[name]), secretFileMode, owner); err != nil {
			return err
		}
	}
	return nil
}

// writeUnit 渲染并落盘 unit，返回渲染结果与「是否真的改动了文件」。
func (a *Adapter) writeUnit(ctx context.Context, spec *domain.ApplicationSpec) (unitfile.Rendered, bool, error) {
	version, err := a.detectVersion(ctx)
	if err != nil {
		return unitfile.Rendered{}, false, err
	}
	// 用已经探测并缓存的版本渲染，而不是 RenderForHost：后者会再探测一次，
	// 让「写进注释的版本」与「本次校验用的版本」变成两个可能不一致的来源。
	rendered, err := unitfile.RenderForVersion(spec, version)
	if err != nil {
		return unitfile.Rendered{}, false, err
	}
	changed, err := a.writeFileIfChanged(
		a.RootPath(domain.UnitPath(spec.Systemd.UnitName)), []byte(rendered.Content), unitMode)
	if err != nil {
		return unitfile.Rendered{}, false, err
	}
	return rendered, changed, nil
}

// writeFile 把内容写到 root 映射后的路径，并收敛模式与属主。
func (a *Adapter) writeFile(spec *domain.ApplicationSpec, absolute string, content []byte, mode os.FileMode, owner ownership) error {
	path := a.RootPath(absolute)
	if _, err := a.writeFileIfChanged(path, content, mode); err != nil {
		return err
	}
	// 属主每次都设：内容没变但属主被别的进程改过时，同样要收敛回去。
	if err := os.Chown(path, owner.uid, owner.gid); err != nil {
		return domain.NewError(v1.CodeInternal, "设置文件 %q 属主失败: %v", path, err)
	}
	return nil
}

// writeFileIfChanged 只在内容或权限与期望不符时才落盘，并报告是否真的写了。
//
// Prepare 幂等的要求不只是「不报错」：内容没变却重写会刷新文件 mtime，
// 对 unit 文件来说这意味着一次没有意义的 daemon-reload。
func (a *Adapter) writeFileIfChanged(path string, content []byte, mode os.FileMode) (bool, error) {
	if existing, err := os.ReadFile(path); err == nil {
		if info, statErr := os.Stat(path); statErr == nil &&
			bytes.Equal(existing, content) && info.Mode().Perm() == mode {
			return false, nil
		}
	}
	dir := filepath.Dir(path)
	if err := a.ensureAncestors(dir); err != nil {
		return false, err
	}
	if err := os.MkdirAll(dir, 0o751); err != nil {
		return false, domain.NewError(v1.CodeInternal, "创建目录 %q 失败: %v", dir, err)
	}
	if err := os.WriteFile(path, content, mode); err != nil {
		return false, domain.NewError(v1.CodeInternal, "写入 %q 失败: %v", path, err)
	}
	// os.WriteFile 不改已存在文件的权限，显式 Chmod 才能让权限真的收敛。
	if err := os.Chmod(path, mode); err != nil {
		return false, domain.NewError(v1.CodeInternal, "收敛文件 %q 权限失败: %v", path, err)
	}
	return true, nil
}

// mustRun 执行一条命令并要求退出码为 0。非零退出码把 stderr 一起带出去：
// 排查 systemd 问题时，错误信息里有没有 stderr 决定了是两分钟还是两小时。
func (a *Adapter) mustRun(ctx context.Context, code v1.ErrorCode, argv ...string) (Result, error) {
	result, err := a.runner(ctx, argv)
	if err != nil {
		return result, domain.NewError(code, "执行 %s 失败: %v", strings.Join(argv, " "), err)
	}
	if result.ExitCode != 0 {
		return result, domain.NewError(code, "%s 退出码 %d: %s",
			strings.Join(argv, " "), result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return result, nil
}

// prepared 用 unit 文件是否存在作为「已经 Prepare 过」的判据。
// 没有 unit 就 Start，等于让 systemd 去跑一个 opsd 完全没有准备过的东西
// （用户、目录、凭据都可能不存在）。
func (a *Adapter) prepared(spec *domain.ApplicationSpec) error {
	exists, err := a.unitFileExists(spec)
	if err != nil {
		return err
	}
	if !exists {
		return domain.NewError(v1.CodeRuntimeNotReady,
			"应用 %s 尚未 Prepare：unit 文件 %s 不存在", spec.Application, domain.UnitPath(spec.Systemd.UnitName))
	}
	return nil
}

func (a *Adapter) unitFileExists(spec *domain.ApplicationSpec) (bool, error) {
	_, err := os.Stat(a.RootPath(domain.UnitPath(spec.Systemd.UnitName)))
	switch {
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, domain.NewError(v1.CodeInternal, "检查 unit 文件失败: %v", err)
	}
}
