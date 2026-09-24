package unitfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// javaSpec 造一份 runtime: java 的规格：解释器绝对路径 + JVM 参数 + jar 都写在 argv 里
// （迭代 3 决定 2 的 A 方案）。
func javaSpec(interpreter string) *domain.ApplicationSpec {
	spec := validSpec()
	spec.Runtime = domain.RuntimeKindJava
	spec.Exec.Argv = []string{interpreter, "-Xmx512m", "-jar", "app.jar"}
	return spec
}

// goSpec 是同一份规格、runtime 换成 go：用来证明这条检查**只对 java**。
func goSpecWithArgv0(argv0 string) *domain.ApplicationSpec {
	spec := validSpec()
	spec.Runtime = domain.RuntimeKindGo
	spec.Exec.Argv = []string{argv0, "--config", "/etc/orders/config.yaml"}
	return spec
}

// fakeExecutable 造一个可执行文件（不需要是真的可执行格式：这里检查的是模式位）。
func fakeExecutable(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("写夹具: %v", err)
	}
	return path
}

func TestPreflightInterpreterAcceptsExecutable(t *testing.T) {
	path := fakeExecutable(t, "java")
	if err := PreflightInterpreter(javaSpec(path)); err != nil {
		t.Fatalf("存在的可执行解释器应当通过: %v", err)
	}
}

// 真实机器上 `/usr/bin/java` 多半是符号链接（alternatives 机制）。用 Lstat 就会把这种
// 完全正常的部署判成「解释器不可用」，因此必须跟随链接。
func TestPreflightInterpreterFollowsSymlink(t *testing.T) {
	target := fakeExecutable(t, "jdk-17.0.1-java")
	link := filepath.Join(t.TempDir(), "java")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := PreflightInterpreter(javaSpec(link)); err != nil {
		t.Fatalf("指向可执行文件的符号链接应当通过: %v", err)
	}
}

// 这就是这条检查存在的理由：JDK 不在那里时，报错必须**在部署之前**、且指向那个路径。
// 否则部署会一路走到「unit 起来了、进程立刻退出」，最后以「就绪超时」收场。
func TestPreflightInterpreterRejectsMissingPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "jdk-17.0.1", "bin", "java")
	err := PreflightInterpreter(javaSpec(missing))
	if domain.CodeOf(err) != v1.CodeManifestInvalid {
		t.Fatalf("want MANIFEST_INVALID, got %v", err)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("报错必须带上那个路径，否则运维不知道该改什么: %v", err)
	}
}

func TestPreflightInterpreterRejectsNonExecutable(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "java")
	if err := os.WriteFile(plain, []byte("not executable"), 0o644); err != nil {
		t.Fatalf("写夹具: %v", err)
	}
	if err := PreflightInterpreter(javaSpec(plain)); domain.CodeOf(err) != v1.CodeManifestInvalid {
		t.Fatalf("没有执行位的解释器必须被拒: %v", err)
	}
	if err := PreflightInterpreter(javaSpec(dir)); domain.CodeOf(err) != v1.CodeManifestInvalid {
		t.Fatalf("目录不能被当成解释器: %v", err)
	}
}

// 相对 argv[0] 指向 release 里的文件，而调用这条检查的时机（Validate / Prepare）
// **早于物化**——那时它按定义还不存在。检查它只会把「正常的第一次部署」判成错误。
// java 也不排除这种用法：把 JRE 打进制品里是合法的。
func TestPreflightInterpreterSkipsRelativeArgv0(t *testing.T) {
	spec := javaSpec("jre/bin/java")
	if err := PreflightInterpreter(spec); err != nil {
		t.Fatalf("相对解释器路径应当跳过检查（由部署的物化保证）: %v", err)
	}
}

// 本轮只对 java 检查：go 的绝对 argv[0] 面临同一类风险，但收紧它会改变 1c 起就存在的
// 形态的校验行为（夹具与真实用法都在用绝对路径），需要单独判断兼容性影响。
func TestPreflightInterpreterIgnoresNonJavaRuntime(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "definitely-not-here")
	if err := PreflightInterpreter(goSpecWithArgv0(missing)); err != nil {
		t.Fatalf("go 的绝对 argv[0] 本轮不检查: %v", err)
	}
	// 空规格与 nil 不该 panic：这条检查会在各处被顺手调用。
	if err := PreflightInterpreter(nil); err != nil {
		t.Fatalf("nil 规格应当返回 nil: %v", err)
	}
	empty := javaSpec("")
	empty.Exec.Argv = nil
	if err := PreflightInterpreter(empty); err != nil {
		t.Fatalf("argv 为空时不应当在这里报错（spec.Validate 会报）: %v", err)
	}
}
