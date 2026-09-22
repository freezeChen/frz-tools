package main

import (
	"fmt"
	"strings"
	"time"

	v1 "frz-tools/api/v1"
	"frz-tools/internal/adapters/client"
	"frz-tools/internal/domain"

	"github.com/spf13/cobra"
)

func newAppCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "app",
		Short: "管理应用",
	}
	cmd.AddCommand(newAppCreateCommand(opts), newAppListCommand(opts), newAppInspectCommand(opts))
	return cmd
}

func newAppCreateCommand(opts *rootOptions) *cobra.Command {
	var labels []string

	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "创建应用",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			parsed, err := parseLabels(labels)
			if err != nil {
				return err
			}
			app, err := opts.client().CreateApplication(cmd.Context(), args[0], parsed)
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(app)
			}
			printApplication(app)
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&labels, "label", nil, "键值对标签，可重复使用，例如 --label env=prod")
	return cmd
}

func newAppListCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出应用",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			response, err := opts.client().ListApplications(cmd.Context())
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(response)
			}
			for i := range response.Items {
				printApplication(&response.Items[i])
				fmt.Println()
			}
			return nil
		},
	}
}

func newAppInspectCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <name-or-id>",
		Short: "查看单个应用",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := opts.client().GetApplication(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(app)
			}
			printApplication(app)
			return nil
		},
	}
}

func newReleaseCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "release",
		Short: "登记与查询发布记录",
	}
	cmd.AddCommand(newReleaseAddCommand(opts), newReleaseListCommand(opts), newReleaseInspectCommand(opts))
	return cmd
}

func newReleaseAddCommand(opts *rootOptions) *cobra.Command {
	var (
		app       string
		artifact  string
		version   string
		createdBy string
		labels    []string
	)

	cmd := &cobra.Command{
		Use:   "add --app <name-or-id> --artifact <id-or-digest> --version <v>",
		Short: "把制品登记为应用的一个版本",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			parsed, err := parseLabels(labels)
			if err != nil {
				return err
			}
			release, err := opts.client().CreateRelease(cmd.Context(), client.CreateReleaseInput{
				Application: app,
				Artifact:    artifact,
				Version:     version,
				Labels:      parsed,
				CreatedBy:   createdBy,
			})
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(release)
			}
			printRelease(release)
			return nil
		},
	}

	cmd.Flags().StringVar(&app, "app", "", "应用名称或 ID（必填）")
	cmd.Flags().StringVar(&artifact, "artifact", "", "制品 ID 或 sha256:<hex>（必填）")
	cmd.Flags().StringVar(&version, "version", "", "应用内版本号（必填）")
	cmd.Flags().StringVar(&createdBy, "created-by", "", "调用方标识")
	cmd.Flags().StringArrayVar(&labels, "label", nil, "键值对标签，可重复使用")
	_ = cmd.MarkFlagRequired("app")
	_ = cmd.MarkFlagRequired("artifact")
	_ = cmd.MarkFlagRequired("version")
	return cmd
}

func newReleaseListCommand(opts *rootOptions) *cobra.Command {
	var (
		app   string
		limit int
	)

	cmd := &cobra.Command{
		Use:   "list --app <name-or-id>",
		Short: "列出应用的发布记录",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			response, err := opts.client().ListReleases(cmd.Context(), app, limit)
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(response)
			}
			for i := range response.Items {
				printRelease(&response.Items[i])
				fmt.Println()
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&app, "app", "", "应用名称或 ID（必填）")
	cmd.Flags().IntVar(&limit, "limit", 0, "返回条数上限")
	_ = cmd.MarkFlagRequired("app")
	return cmd
}

func newReleaseInspectCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <release-id>",
		Short: "查看单个发布记录",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			release, err := opts.client().GetRelease(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(release)
			}
			printRelease(release)
			return nil
		},
	}
}

// parseLabels 解析 k=v 形式的旗标；值里可以继续出现等号，所以只切第一处。
func parseLabels(raw []string) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	labels := make(map[string]string, len(raw))
	for _, item := range raw {
		key, value, found := strings.Cut(item, "=")
		if !found || strings.TrimSpace(key) == "" {
			return nil, domain.NewError(v1.CodeInvalidRequest, "--label 的 %q 必须是 key=value 形式", item)
		}
		labels[key] = value
	}
	return labels, nil
}

func printApplication(app *v1.Application) {
	fmt.Printf("id:        %s\n", app.ID)
	fmt.Printf("name:      %s\n", app.Name)
	printLabels(app.Labels)
	fmt.Printf("createdAt: %s\n", app.CreatedAt.Format(time.RFC3339))
}

func printRelease(release *v1.Release) {
	fmt.Printf("id:          %s\n", release.ID)
	fmt.Printf("application: %s\n", release.ApplicationID)
	fmt.Printf("artifact:    %s\n", release.ArtifactID)
	fmt.Printf("version:     %s\n", release.Version)
	printLabels(release.Labels)
	fmt.Printf("createdAt:   %s\n", release.CreatedAt.Format(time.RFC3339))
}

func printLabels(labels map[string]string) {
	for key, value := range labels {
		fmt.Printf("label:      %s=%s\n", key, value)
	}
}
