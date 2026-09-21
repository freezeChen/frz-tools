package main

import (
	"fmt"

	"frz-tools/internal/adapters/config"

	"github.com/spf13/cobra"
)

func newConfigCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Work with opsd configuration files",
	}

	var file string
	validate := &cobra.Command{
		Use:   "validate --file <path>",
		Short: "Validate an opsd configuration file without contacting opsd",
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
			fmt.Printf("%s is valid\n", file)
			return nil
		},
	}
	validate.Flags().StringVar(&file, "file", "", "path to the opsd configuration file (required)")
	_ = validate.MarkFlagRequired("file")

	cmd.AddCommand(validate)
	return cmd
}
