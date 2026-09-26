package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/client"
	"github.com/freezeChen/frz-tools/internal/domain"

	"github.com/spf13/cobra"
)

func newAppCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "app",
		Short: "管理应用",
	}
	cmd.AddCommand(
		newAppCreateCommand(opts),
		newAppListCommand(opts),
		newAppInspectCommand(opts),
		// 部署与回滚（迭代 3）：它们直接操作应用，不挂在 release 下面——release 是
		// 「登记一条版本记录」，而部署是「把某个版本真正跑起来」。
		newAppDeployCommand(opts),
		newAppRollbackCommand(opts),
	)
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

func newAppDeployCommand(opts *rootOptions) *cobra.Command {
	flags := &actionFlags{}
	var (
		app  string
		file string
	)

	cmd := &cobra.Command{
		Use:   "deploy --app <name> --file <manifest.yaml>",
		Short: "部署一个版本：物化制品、准备运行时、切换并启动",
		Long: "把 manifest 里声明的制品解成一个带版本的 release 目录，然后切换 current 指针、" +
			"启动并等就绪。**健康通过才算部署成功**；任何一步失败都会把上一个稳定版本放回去，" +
			"并以 DEPLOY_ROLLED_BACK（退出码 29）收场。\n\n" +
			"同一版本重复部署是幂等的：它已经是当前版本时什么都不做。",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if app == "" || file == "" {
				return domain.NewError(v1.CodeInvalidRequest, "必须给出 --app 与 --file")
			}
			raw, err := os.ReadFile(file)
			if err != nil {
				return domain.NewError(v1.CodeInvalidRequest, "无法读取 manifest %q：%v", file, err)
			}
			in, err := flags.input(cmd)
			if err != nil {
				return err
			}
			result, err := opts.client().DeployApplication(cmd.Context(), app, string(raw), in)
			if err != nil {
				return err
			}
			return reportDeploy(opts, result, "部署")
		},
	}

	cmd.Flags().StringVar(&app, "app", "", "应用名称或 ID（必填）")
	cmd.Flags().StringVar(&file, "file", "", "应用 manifest 文件路径（必填）")
	flags.bind(cmd)
	return cmd
}

func newAppRollbackCommand(opts *rootOptions) *cobra.Command {
	flags := &actionFlags{}
	var (
		app string
		to  string
	)

	cmd := &cobra.Command{
		Use:   "rollback --app <name> [--to <version>]",
		Short: "回滚到上一个（或指定的）版本",
		Long: "回滚**连配置一起回滚**：每个 release 都记着它当时那份 manifest，回滚时用的是它，\n" +
			"不会出现「旧二进制配新配置」的混合体。不带 --to 时回到上一个曾经激活过的版本。",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if app == "" {
				return domain.NewError(v1.CodeInvalidRequest, "必须给出 --app")
			}
			in, err := flags.input(cmd)
			if err != nil {
				return err
			}
			result, err := opts.client().RollbackApplication(cmd.Context(), app, to, in)
			if err != nil {
				return err
			}
			return reportDeploy(opts, result, "回滚")
		},
	}

	cmd.Flags().StringVar(&app, "app", "", "应用名称或 ID（必填）")
	cmd.Flags().StringVar(&to, "to", "", "回滚到哪个版本（省略则回到上一个曾经激活过的版本）")
	flags.bind(cmd)
	return cmd
}

// reportDeploy 打印部署/回滚的结果。
//
// 两种结果都要说清楚：**提交了但没有操作**（版本已经是当前版本）与**产生了操作**是两件事，
// 而它们都不等于"已经成功"——部署的成败在 Operation 的终态上。
func reportDeploy(opts *rootOptions, result *v1.DeployResponse, action string) error {
	if opts.json {
		return opts.printJSON(result)
	}
	if result.Noop {
		fmt.Printf("无需%s：版本 %s 已经是当前版本（release %s）\n", action, result.Release.Version, result.Release.ID)
		return nil
	}
	fmt.Printf("已提交%s：release %s（版本 %s）\n", action, result.Release.ID, result.Release.Version)
	// 蓝绿应用才有的信息：这一版上到了哪一侧。切流是异步的（走 Operation），
	// 因此这里说的是「目标槽位」，不是「流量已经切过去了」。
	if result.Release.Slot != "" {
		fmt.Printf("目标槽位：%s（切流进度见下面的操作日志）\n", result.Release.Slot)
	}
	if result.Operation != nil {
		printOperation(result.Operation)
		fmt.Printf("\n查询进度：opsctl operation get %s\n查看日志：opsctl operation logs %s\n",
			result.Operation.ID, result.Operation.ID)
	}
	return nil
}
