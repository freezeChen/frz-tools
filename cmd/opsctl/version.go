package main

import (
	"fmt"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
)

// newVersionCommand 打印**客户端**的构建版本（迭代 6 规格 D3 之后的 CLI 表格）。
//
// 默认不连 daemon、退出码恒 0：查版本是最经常在「环境是坏的」时做出的动作，
// 它不该再依赖一个可能连不上的 opsd。想知道对端版本是显式选择——`--daemon`
// 才连，连不上打一行标注仍然退出 0，把「可能慢/可能失败」的路径变成显式的。
func newVersionCommand(opts *rootOptions) *cobra.Command {
	var daemon bool

	cmd := &cobra.Command{
		Use:   "version",
		Short: "打印 opsctl 的构建版本",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Printf("opsctl: %s\n", displayVersion(buildVersion()))
			if !daemon {
				return nil
			}
			identity, err := opts.client().Identity(cmd.Context())
			if err != nil {
				fmt.Printf("opsd:   对端不可达（%v）\n", err)
				return nil
			}
			fmt.Printf("opsd:   %s\n", displayVersion(identity.DaemonVersion))
			return nil
		},
	}

	cmd.Flags().BoolVar(&daemon, "daemon", false, "同时连接 opsd（受 --socket / --remote 影响）并追加对端版本；连不上仍以 0 退出")
	return cmd
}

// buildVersion 从构建信息里拼出版本号：模块版本 + VCS 修订。与 opsd 侧
// （cmd/opsd/main.go 的同名函数）同一取法，刻意不加 -ldflags 变量：go build 在
// git 仓库里默认会写入 vcs.revision，因此「这个二进制是哪个提交编的」不需要
// 额外的构建约定就能答出来。取不到时返回空串——空串是诚实的事实（没有信息），
// 不是 "unknown"（那会被读成「查过了、查不到」）。
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var parts []string
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		parts = append(parts, info.Main.Version)
	}
	for _, setting := range info.Settings {
		if setting.Key != "vcs.revision" || setting.Value == "" {
			continue
		}
		revision := setting.Value
		if len(revision) > 12 {
			revision = revision[:12]
		}
		parts = append(parts, revision)
		break
	}
	return strings.Join(parts, "+")
}
