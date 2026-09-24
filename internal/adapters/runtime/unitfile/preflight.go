package unitfile

import (
	"os"
	"path"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// PreflightInterpreter 检查 `runtime: java` 的解释器在本机是否真的能被启动
// （迭代 3c：Java 的「解释器路径的校验」）。
//
// 为什么只有 java 需要这一步：**java 的 argv[0] 是主机上的解释器，不是制品里的东西**。
// go 的推荐形态是相对 argv[0]（制品里的可执行文件），部署会把它解出来，因此「它不存在」
// 在物化那一步就失败了；而 JDK 不由部署物化、也不由 Prepare 安装——**没有任何环节会检查
// 它**。写错了（JDK 路径变了、这台机器上根本没装）的表现是：unit 起来了、进程立刻退出、
// systemd 反复重启、最后报「就绪超时」。那句报错与真实原因毫无关系，而排查它往往要从
// `journalctl -u` 里翻出 `No such file or directory` 才回到正题。
//
// 三条边界：
//
//   - **只对 java**。go 的绝对 argv[0] 面临同一类风险，但本轮不动它：那会改变 1c 起就
//     存在的形态的校验行为（收紧），需要单独判断兼容性影响，已记入停放区。
//   - **只对绝对 argv[0]**。相对 argv[0] 落在 release 的 current 之下，而调用它的时机
//     （Validate / Prepare）**早于物化**，那时它按定义还不存在——检查它只会把「正常的
//     第一次部署」判成错误。
//   - **不做 root 前缀重定向**（对比 RootPath）。root 前缀是给「我们管理的生产路径」用的
//     测试手段，而解释器不生活在那套路径空间里：把它映射到临时目录只会问出一个测试形状
//     的答案。
//
// 用 Stat 而不是 Lstat：`/usr/bin/java` 在真实机器上多半是指向具体 JDK 的符号链接
// （alternatives 机制），不跟随链接就会把它判成「不可用」。
func PreflightInterpreter(spec *domain.ApplicationSpec) error {
	if spec == nil || spec.Runtime != domain.RuntimeKindJava || len(spec.Exec.Argv) == 0 {
		return nil
	}
	interpreter := spec.Exec.Argv[0]
	if !path.IsAbs(interpreter) {
		return nil
	}

	info, err := os.Stat(interpreter)
	if err != nil {
		return domain.NewError(v1.CodeManifestInvalid,
			"runtime: java 的解释器 %q 在本机不可用（%v）：它不在制品里，部署不会创建它，因此必须在目标主机上已存在",
			interpreter, err)
	}
	if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return domain.NewError(v1.CodeManifestInvalid,
			"runtime: java 的解释器 %q 不是可执行文件（模式 %04o）", interpreter, info.Mode().Perm())
	}
	return nil
}
