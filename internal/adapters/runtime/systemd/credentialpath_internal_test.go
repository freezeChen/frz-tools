package systemd

import (
	"os"
	"testing"
)

// traversable 是「runUser 能否穿过这一级目录」的判定规则，也是这条链上唯一可能写错的
// 决策：它按属主 / 主组 / 其他三类位判定，只看 +x。
//
// 这一层用包内测试直接钉住，是因为完整失败路径在无特权环境里造不出来：要让一个目录
// 「不是我们的、又对 runUser 不可穿越」，需要把它的属主改成别的用户，而那要 root。
// 真实的跨用户读取结果记为 A7（Linux 容器）的断言。
func TestTraversable(t *testing.T) {
	runUser := ownership{uid: 1001, gid: 1001}

	cases := []struct {
		name   string
		mode   os.FileMode
		dirUID int
		dirGID int
		want   bool
	}{
		{name: "其他用户且 other 有 +x", mode: 0o751, dirUID: 0, dirGID: 0, want: true},
		{name: "其他用户且 other 无 +x", mode: 0o750, dirUID: 0, dirGID: 0, want: false},
		{name: "其他用户且 other 只有读位", mode: 0o754, dirUID: 0, dirGID: 0, want: false},
		{name: "运行用户是属主且有 +x", mode: 0o700, dirUID: runUser.uid, dirGID: 0, want: true},
		{name: "运行用户是属主但无 +x", mode: 0o600, dirUID: runUser.uid, dirGID: 0, want: false},
		{name: "运行用户在组里且有 +x", mode: 0o750, dirUID: 0, dirGID: runUser.gid, want: true},
		{name: "运行用户在组里但无 +x", mode: 0o740, dirUID: 0, dirGID: runUser.gid, want: false},
	}

	for _, tc := range cases {
		if got := traversable(tc.mode, tc.dirUID, tc.dirGID, runUser); got != tc.want {
			t.Fatalf("%s: traversable(%04o, %d, %d) = %v, want %v",
				tc.name, tc.mode, tc.dirUID, tc.dirGID, got, tc.want)
		}
	}
}

// canChmod 决定「这一级我们能不能自己修」。真实容器里 opsd 以 root 跑、而 /etc/opsd
// 属主是服务用户 frz-ops，只看「属主等于自己」会漏掉这一支，让 Prepare 在正常部署上失败。
func TestCanChmod(t *testing.T) {
	runUser := ownership{uid: 1001, gid: 1001}

	cases := []struct {
		name   string
		euid   int
		dirUID int
		want   bool
	}{
		{name: "root 可以收敛任何目录", euid: 0, dirUID: 995, want: true},
		{name: "非 root 且是属主", euid: runUser.uid, dirUID: runUser.uid, want: true},
		{name: "非 root 且不是属主", euid: runUser.uid, dirUID: 995, want: false},
	}
	for _, tc := range cases {
		if got := canChmod(tc.euid, tc.dirUID); got != tc.want {
			t.Fatalf("%s: canChmod(%d, %d) = %v, want %v", tc.name, tc.euid, tc.dirUID, got, tc.want)
		}
	}
}

// 凭据路径必须从根到叶、且以凭据目录结尾：顺序决定错误信息指向哪一级，
// 也决定「哪一级不参与补穿越位」（末项）。
func TestCredentialPathLevels(t *testing.T) {
	got := credentialPathLevels("orders", "")
	want := []string{
		"/etc",
		"/etc/opsd",
		"/etc/opsd/apps",
		"/etc/opsd/apps/orders.secrets",
	}
	if len(got) != len(want) {
		t.Fatalf("层级数 = %d (%v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 级 = %q, want %q（必须是从根到叶）", i, got[i], want[i])
		}
	}
}
