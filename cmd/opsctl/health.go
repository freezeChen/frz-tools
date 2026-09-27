package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func newHealthCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "health",
		Short: "检查 opsd 及其数据库是否可达",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			health, err := opts.client().Health(cmd.Context())
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(health)
			}
			fmt.Printf("daemon:   %s\n", health.Daemon)
			fmt.Printf("database: %s\n", health.Database)
			fmt.Printf("workers:  %d\n", health.Workers)
			fmt.Printf("uptime:   %s\n", time.Duration(health.UptimeMS)*time.Millisecond)
			return nil
		},
	}
}

// newIdentityCommand 回答「我打到了哪台机、以什么身份」（迭代 5a）。
//
// 它与 health 是两件事：health 问「你活着吗」（会去 ping 数据库），identity 问
// 「你是谁」。用 --remote 打过去之后，先跑这条命令是排查的第一步——backend 是不是
// tls、clientCn 是不是我以为的那张证书、scope 够不够，都在这三行里。
func newIdentityCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "identity",
		Short: "查看对端 opsd 的身份与本次连接的身份",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			identity, err := opts.client().Identity(cmd.Context())
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(identity)
			}
			fmt.Printf("target:    %s\n", opts.client().Target())
			fmt.Printf("hostname:  %s\n", identity.Hostname)
			fmt.Printf("version:   %s\n", displayVersion(identity.DaemonVersion))
			fmt.Printf("backend:   %s\n", identity.Backend)
			fmt.Printf("clientCn:  %s\n", displayClientCn(identity))
			fmt.Printf("scope:     %s\n", identity.Scope)
			fmt.Printf("applications: %s\n", displayCount(identity.Applications))
			if len(identity.ApplicationsAllowed) == 0 {
				fmt.Println("allowed:   （对端上的全部应用）")
			} else {
				fmt.Printf("allowed:   %s\n", strings.Join(identity.ApplicationsAllowed, ", "))
			}
			return nil
		},
	}
}
