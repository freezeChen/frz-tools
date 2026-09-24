package unitfile

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// fakeRunner 记录收到的 argv 并返回固定输出。探测测试一律走它，
// 绝不真去 exec 本机 systemctl：那既是环境测试，在 macOS 上也根本跑不通。
func fakeRunner(stdout string, err error, seen *[]string) VersionRunner {
	return func(_ context.Context, argv []string) ([]byte, error) {
		if seen != nil {
			*seen = append([]string(nil), argv...)
		}
		return []byte(stdout), err
	}
}

func TestParseSystemctlVersion(t *testing.T) {
	cases := []struct {
		name    string
		output  string
		want    int
		wantErr bool
	}{
		{name: "systemd 255 带能力行", output: "systemd 255\n+PAM +AUDIT +SELINUX +APPARMOR\n", want: 255},
		{name: "带发行版补丁号", output: "systemd 252 (252.5-2~deb12u1)\n+PAM\n", want: 252},
		{name: "CentOS 7 的 219", output: "systemd 219\n+PAM +AUDIT +SELINUX +IMA -APPARMOR\n", want: 219},
		{name: "CRLF 行尾", output: "systemd 240\r\n+PAM\r\n", want: 240},
		{name: "只有数字形态的前缀行", output: "systemd 250\n", want: 250},
		{name: "垃圾输出", output: "command not found\n", wantErr: true},
		{name: "空输出", output: "", wantErr: true},
		{name: "只有 systemd 没有版本", output: "systemd\n", wantErr: true},
		{name: "版本不是十进制", output: "systemd v255\n", wantErr: true},
		{name: "非 systemd 首行", output: "systemd-sysv 1.2\n", wantErr: true},
	}
	for _, tc := range cases {
		got, err := ParseSystemctlVersion(tc.output)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("%s: 必须报错，却得到 %d", tc.name, got)
			}
			if code := domain.CodeOf(err); code != v1.CodeRuntimeUnsupport {
				t.Fatalf("%s: 错误码 = %s, want RUNTIME_UNSUPPORTED", tc.name, code)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: 解析失败: %v", tc.name, err)
		}
		if got != tc.want {
			t.Fatalf("%s: 解析得到 %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestProberDetect(t *testing.T) {
	cases := []struct {
		name    string
		stdout  string
		runErr  error
		want    int
		wantErr bool
	}{
		{name: "新档主机", stdout: "systemd 255\n+PAM +AUDIT\n", want: 255},
		{name: "旧档主机", stdout: "systemd 219\n+PAM\n", want: 219},
		{name: "边界 240", stdout: "systemd 240\n", want: 240},
		{name: "边界 239", stdout: "systemd 239\n", want: 239},
		{name: "低于支持范围", stdout: "systemd 218\n", wantErr: true},
		{name: "输出不可解析", stdout: "bash: systemctl: command not found\n", wantErr: true},
		{name: "命令失败", runErr: errors.New("exit status 1"), wantErr: true},
		{name: "命令超时", runErr: context.DeadlineExceeded, wantErr: true},
	}
	for _, tc := range cases {
		var seen []string
		prober := NewProber(fakeRunner(tc.stdout, tc.runErr, &seen))
		got, err := prober.Detect(context.Background())
		if tc.wantErr {
			if err == nil {
				t.Fatalf("%s: 必须报错，却得到 %d", tc.name, got)
			}
			// 探测失败绝不能被「默认成 strict」消化掉：这里要求错误码统一为 RUNTIME_UNSUPPORTED。
			if code := domain.CodeOf(err); code != v1.CodeRuntimeUnsupport {
				t.Fatalf("%s: 错误码 = %s, want RUNTIME_UNSUPPORTED", tc.name, code)
			}
		} else {
			if err != nil {
				t.Fatalf("%s: 探测失败: %v", tc.name, err)
			}
			if got != tc.want {
				t.Fatalf("%s: 探测得到 %d, want %d", tc.name, got, tc.want)
			}
			if _, err := TierFor(got); err != nil {
				t.Fatalf("%s: 探测结果必须能选出档位: %v", tc.name, err)
			}
		}
		// 探测命令固定为 argv-only 的 systemctl --version，不经过 shell。
		if len(seen) != 2 || seen[0] != "systemctl" || seen[1] != "--version" {
			t.Fatalf("%s: 探测命令不对: %v", tc.name, seen)
		}
	}
}

func TestProberDetectWithoutRunner(t *testing.T) {
	if _, err := NewProber(nil).Detect(context.Background()); err == nil {
		t.Fatal("没有 runner 时必须报错，而不是假定 strict")
	}
	var p *Prober
	if _, err := p.Detect(context.Background()); err == nil {
		t.Fatal("nil 探测器必须报错")
	}
}

func TestProberDetectPropagatesContext(t *testing.T) {
	type ctxKey struct{}
	want := errors.New("sentinel")
	prober := NewProber(func(ctx context.Context, _ []string) ([]byte, error) {
		if ctx.Value(ctxKey{}) != "trace" {
			t.Errorf("runner 未收到调用方的 context")
		}
		return nil, want
	})
	ctx := context.WithValue(context.Background(), ctxKey{}, "trace")
	_, err := prober.Detect(ctx)
	if err == nil || !strings.Contains(err.Error(), want.Error()) {
		t.Fatalf("底层错误必须带上原因: %v", err)
	}
}

func TestCommandRunnerIsArgvOnly(t *testing.T) {
	// CommandRunner 是生产路径（真机上探测 systemctl），所以这里用真实进程跑一次：
	// 只验 argv 直传与 stdout 捕获，不依赖本机有 systemctl。
	const echo = "/bin/echo"
	if _, err := os.Stat(echo); err != nil {
		t.Skipf("%s 不存在，跳过真实进程冒烟", echo)
	}
	// argv 里带分号与引号：若实现意外经过 shell，输出不会是这里期望的字节。
	out, err := CommandRunner(context.Background(), []string{echo, "systemd", "255", ";", `"quoted"`})
	if err != nil {
		t.Fatalf("CommandRunner 报错: %v", err)
	}
	if got := string(out); got != "systemd 255 ; \"quoted\"\n" {
		t.Fatalf("stdout = %q", got)
	}

	// 用固定 argv 的探测器串起「执行→解析→选档→渲染」的完整链路。
	prober := &Prober{run: CommandRunner, argv: []string{echo, "systemd", "255"}}
	got, err := RenderForHost(context.Background(), validSpec(), prober)
	if err != nil {
		t.Fatalf("RenderForHost 报错: %v", err)
	}
	if got.Tier != TierStrict || got.SystemdVersion != 255 {
		t.Fatalf("审计信息不对: tier=%q version=%d", got.Tier, got.SystemdVersion)
	}
	if got.Content != strictUnit {
		t.Fatalf("链路产物不符:\n%s", got.Content)
	}
}

func TestRenderForHostUsesProbedVersion(t *testing.T) {
	spec := validSpec()

	newHost := NewProber(fakeRunner("systemd 255\n+PAM\n", nil, nil))
	got, err := RenderForHost(context.Background(), spec, newHost)
	if err != nil {
		t.Fatalf("RenderForHost 报错: %v", err)
	}
	if got.Content != strictUnit {
		t.Fatalf("255 主机应渲染 strict 档:\n%s", got.Content)
	}

	oldHost := NewProber(fakeRunner("systemd 219\n+PAM\n", nil, nil))
	got, err = RenderForHost(context.Background(), spec, oldHost)
	if err != nil {
		t.Fatalf("RenderForHost 报错: %v", err)
	}
	if got.Tier != TierLegacy || got.SystemdVersion != 219 {
		t.Fatalf("审计信息不对: tier=%q version=%d", got.Tier, got.SystemdVersion)
	}
	if !strings.Contains(got.Content, "StandardOutput=journal\n") {
		t.Fatalf("219 主机应渲染 legacy 档:\n%s", got.Content)
	}

	// 探测失败时渲染必须整体失败，不能退回某个默认档。
	broken := NewProber(fakeRunner("", errors.New("no systemctl"), nil))
	if _, err := RenderForHost(context.Background(), spec, broken); err == nil {
		t.Fatal("探测失败时不得渲染出任何 unit")
	}
}
