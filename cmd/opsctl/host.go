package main

import (
	"fmt"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"

	"github.com/spf13/cobra"
)

func newHostCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "host",
		Short: "管理主机记录：1c 只建身份与标签，地址为空表示本机",
	}
	cmd.AddCommand(newHostListCommand(opts), newHostCreateCommand(opts), newHostInspectCommand(opts))
	return cmd
}

func newHostInspectCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <name-or-id>",
		Short: "查看单个主机",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			host, err := opts.client().GetHost(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(host)
			}
			printHost(host)
			return nil
		},
	}
}

func newHostListCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出主机",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			response, err := opts.client().ListHosts(cmd.Context())
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(response)
			}
			for i := range response.Items {
				printHost(&response.Items[i])
				fmt.Println()
			}
			return nil
		},
	}
}

func newHostCreateCommand(opts *rootOptions) *cobra.Command {
	var (
		address string
		labels  []string
	)

	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "创建主机记录",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			parsed, err := parseLabels(labels)
			if err != nil {
				return err
			}
			host, err := opts.client().CreateHost(cmd.Context(), args[0], address, parsed)
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(host)
			}
			printHost(host)
			return nil
		},
	}

	cmd.Flags().StringVar(&address, "address", "", "主机地址；留空表示本机")
	cmd.Flags().StringArrayVar(&labels, "label", nil, "键值对标签，可重复使用，例如 --label env=prod")
	return cmd
}

func newEnvCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "管理环境记录：1c 只建身份与标签",
	}
	cmd.AddCommand(newEnvListCommand(opts), newEnvCreateCommand(opts), newEnvInspectCommand(opts))
	return cmd
}

func newEnvInspectCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <name-or-id>",
		Short: "查看单个环境",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			environment, err := opts.client().GetEnvironment(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(environment)
			}
			printEnvironment(environment)
			return nil
		},
	}
}

func newEnvListCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出环境",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			response, err := opts.client().ListEnvironments(cmd.Context())
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(response)
			}
			for i := range response.Items {
				printEnvironment(&response.Items[i])
				fmt.Println()
			}
			return nil
		},
	}
}

func newEnvCreateCommand(opts *rootOptions) *cobra.Command {
	var labels []string

	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "创建环境记录",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			parsed, err := parseLabels(labels)
			if err != nil {
				return err
			}
			environment, err := opts.client().CreateEnvironment(cmd.Context(), args[0], parsed)
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(environment)
			}
			printEnvironment(environment)
			return nil
		},
	}

	cmd.Flags().StringArrayVar(&labels, "label", nil, "键值对标签，可重复使用，例如 --label tier=web")
	return cmd
}

// address 为空是本机的语义，因此这里显式写出「本机」而不是留白。
func printHost(host *v1.Host) {
	fmt.Printf("id:        %s\n", host.ID)
	fmt.Printf("name:      %s\n", host.Name)
	if host.Address == "" {
		fmt.Println("address:   （本机）")
	} else {
		fmt.Printf("address:   %s\n", host.Address)
	}
	printLabels(host.Labels)
	fmt.Printf("createdAt: %s\n", host.CreatedAt.Format(time.RFC3339))
}

func printEnvironment(environment *v1.Environment) {
	fmt.Printf("id:        %s\n", environment.ID)
	fmt.Printf("name:      %s\n", environment.Name)
	printLabels(environment.Labels)
	fmt.Printf("createdAt: %s\n", environment.CreatedAt.Format(time.RFC3339))
}
