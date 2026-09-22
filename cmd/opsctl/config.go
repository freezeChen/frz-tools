package main

import (
	"fmt"

	"frz-tools/internal/adapters/config"

	"github.com/spf13/cobra"
)

func newConfigCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "操作 opsd 的配置文件",
	}

	var file string
	validate := &cobra.Command{
		Use:   "validate --file <path>",
		Short: "校验一个 opsd 配置文件，不连接 opsd",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(file)
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(map[string]any{
					"apiVersion": cfg.APIVersion,
					"kind":       cfg.Kind,
					"valid":      true,
					"socket":     cfg.Socket.Path,
					"database":   cfg.Database.Path,
					"workers":    cfg.Runtime.Workers,
				})
			}
			fmt.Printf("%s 校验通过\n", file)
			return nil
		},
	}
	validate.Flags().StringVar(&file, "file", "", "opsd 配置文件路径（必填）")
	_ = validate.MarkFlagRequired("file")

	cmd.AddCommand(validate)
	return cmd
}
