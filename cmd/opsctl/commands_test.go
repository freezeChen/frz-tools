package main

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/freezeChen/frz-tools/internal/domain"
)

// 未知的子命令必须**报错**，而不是打印一遍帮助然后以 0 退出。
//
// cobra 的默认行为就是后者，而它的后果是：`opsctl artifact push`（正确名字是 put）
// 在脚本里表现为「命令成功」。这个仓库最在意的就是这种静默成功。
func TestUnknownSubcommandFailsInsteadOfPrintingHelp(t *testing.T) {
	for _, args := range [][]string{
		{"artifact", "push"},        // 手滑：真正的名字是 put
		{"app", "deply"},            // 手滑：真正的名字是 deploy
		{"app", "target", "ad"},     // 嵌套一层
		{"host", "no-such-command"}, // 顶层父命令
	} {
		root := newRootCommand()
		root.SetArgs(args)
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)

		err := root.Execute()
		if err == nil {
			t.Fatalf("%v 应当报错，却成功退出了（cobra 的默认行为是打帮助 + 退出码 0）", args)
		}
		if code := domain.CodeOf(err); code != "INVALID_REQUEST" {
			t.Fatalf("%v 的错误码应当是 INVALID_REQUEST，得到 %s（%v）", args, code, err)
		}
		// 报错要指出**那个写错的词**，否则等于没说。
		if !strings.Contains(domain.MessageOf(err), args[len(args)-1]) {
			t.Fatalf("%v 的报错里应当回显那个子命令：%v", args, err)
		}
	}
}

// 光写父命令名仍然打印帮助并成功——那是正常用法，不是错误。
func TestBareParentCommandStillPrintsHelp(t *testing.T) {
	root := newRootCommand()
	root.SetArgs([]string{"app"})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)

	if err := root.Execute(); err != nil {
		t.Fatalf("只写父命令名不该报错：%v", err)
	}
	if !strings.Contains(out.String(), "可用命令") {
		t.Fatalf("应当打印帮助：%s", out.String())
	}
}
