package main

import (
	"fmt"
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
