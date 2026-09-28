package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/client"
	"github.com/freezeChen/frz-tools/internal/adapters/manifest"
	"github.com/freezeChen/frz-tools/internal/domain"
	"github.com/freezeChen/frz-tools/internal/fleet"

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
		// 槽位的运营视图与切换时间线（迭代 4c）。挂在 app 下而不是独立命令：
		// 槽位是**应用**的属性，脱离应用没有意义。
		newAppSlotCommand(opts),
		// 部署目标（迭代 5b）：一个应用**应该**跑在哪些主机上。同样挂在 app 下——
		// 它是应用的属性，而「哪些机器」是它的取值范围。
		newTargetCommand(opts),
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
	batch := &batchFlags{}
	var (
		app  string
		file string
	)

	cmd := &cobra.Command{
		Use:   "deploy --app <name> --file <manifest.yaml> [--to <主机...>]",
		Short: "部署一个版本：物化制品、准备运行时、切换并启动",
		Long: "把 manifest 里声明的制品解成一个带版本的 release 目录，然后切换 current 指针、" +
			"启动并等就绪。**健康通过才算部署成功**；任何一步失败都会把上一个稳定版本放回去，" +
			"并以 DEPLOY_ROLLED_BACK（退出码 29）收场。\n\n" +
			"同一版本重复部署是幂等的：它已经是当前版本时什么都不做。\n\n" +
			"给了 --to 就是**批量部署**：同一个版本按批次推到多台主机上。批量模式下 manifest " +
			"必须按 artifact.digest 引用制品（art_xxx 只在某一台机上有效），而且**先全员准备、" +
			"再开始第一批**——任何一台连不上、没登记这个应用或缺制品时，一台都不会动。",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if app == "" || file == "" {
				return domain.NewError(v1.CodeInvalidRequest, "必须给出 --app 与 --file")
			}
			raw, err := os.ReadFile(file)
			if err != nil {
				return domain.NewError(v1.CodeInvalidRequest, "无法读取 manifest %q：%v", file, err)
			}
			if batch.enabled() {
				return startDeployBatch(cmd, opts, app, raw, flags, batch)
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
	batch.bind(cmd)
	return cmd
}

// startDeployBatch 是批量部署在提交任何东西之前的那一段。
//
// 它拦下两类会在多台机上放大后果的错误：manifest 用 art_xxx 引用制品（那个 ID 只在
// 某一台机上有意义），以及本地制品文件与 manifest 里的摘要对不上（否则五台机都会
// 装上错的制品，而每台机上的部署都会成功——它们的校验对着的是自己刚收到的那份字节）。
func startDeployBatch(cmd *cobra.Command, opts *rootOptions, app string, raw []byte, flags *actionFlags, batch *batchFlags) error {
	if err := rejectIncompatibleBatchFlags(cmd, flags); err != nil {
		return err
	}

	spec, err := manifest.Parse(raw)
	if err != nil {
		return err
	}
	digest := strings.TrimSpace(spec.Artifact.Digest)
	if digest == "" {
		return domain.NewError(v1.CodeInvalidRequest,
			"--to 模式下 manifest 必须按 artifact.digest 引用制品，不能写 artifact.id："+
				"art_xxx 是某一台机上的那一行，换一台机要么找不到、要么指向别的东西；"+
				"而 digest 是内容寻址的，在每台机上指同一份字节")
	}

	artifactPath := batch.artifact
	if artifactPath != "" {
		if err := verifyLocalArtifact(artifactPath, digest); err != nil {
			return err
		}
	}

	return runBatch(cmd.Context(), opts, batch, fleet.Request{
		Action:       fleet.ActionDeploy,
		Application:  app,
		Manifest:     string(raw),
		Digest:       digest,
		ArtifactPath: artifactPath,
		CreatedBy:    flags.createdBy,
	})
}

func newAppRollbackCommand(opts *rootOptions) *cobra.Command {
	flags := &actionFlags{}
	batch := &batchFlags{}
	var (
		app string
		to  string
	)

	cmd := &cobra.Command{
		Use:   "rollback --app <name> [--to <version>] [--hosts <主机...>]",
		Short: "回滚到上一个（或指定的）版本",
		Long: "回滚**连配置一起回滚**：每个 release 都记着它当时那份 manifest，回滚时用的是它，\n" +
			"不会出现「旧二进制配新配置」的混合体。不带 --to 时回到上一个曾经激活过的版本。\n\n" +
			"给了 --hosts 就是**批量回滚**，与批量部署共用同一套批次、准备与汇总逻辑。" +
			"批量目标刻意不叫 --to：那个旗标在回滚上已经是「回到哪个版本」，一个旗标两种含义\n" +
			"会让 `--to 1.0.0` 与 `--to web-1` 长得一模一样，而它们要做的事完全不同。",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if app == "" {
				return domain.NewError(v1.CodeInvalidRequest, "必须给出 --app")
			}
			if batch.enabled() {
				if err := rejectIncompatibleBatchFlags(cmd, flags); err != nil {
					return err
				}
				return runBatch(cmd.Context(), opts, batch, fleet.Request{
					Action:      fleet.ActionRollback,
					Application: app,
					Version:     to,
					CreatedBy:   flags.createdBy,
				})
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
	batch.bind(cmd)
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
	// 蓝绿应用才有的信息：这一次会上到哪一侧、以及它是不是一次切流。
	// 切流是异步的（走 Operation），因此这里说的是「目标槽位」与「这次有没有切流这个动作」，
	// **不是**「流量已经切过去了」。
	if result.Slot != "" {
		if result.Switching {
			fmt.Printf("目标槽位：%s（本次包含一次切流，进度见下面的操作日志）\n", result.Slot)
		} else {
			fmt.Printf("目标槽位：%s（首次部署：还没有流量可切）\n", result.Slot)
		}
	}
	if result.Operation != nil {
		printOperation(result.Operation)
		fmt.Printf("\n查询进度：opsctl operation get %s\n查看日志：opsctl operation logs %s\n",
			result.Operation.ID, result.Operation.ID)
	}
	return nil
}
