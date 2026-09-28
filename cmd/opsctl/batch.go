package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/client"
	"github.com/freezeChen/frz-tools/internal/domain"
	"github.com/freezeChen/frz-tools/internal/fleet"
	"github.com/freezeChen/frz-tools/internal/idgen"

	"github.com/spf13/cobra"
)

// batchFlags 是批量发布的旗标（迭代 5b）。
//
// 它们是**可选**的一组：不给 `--to` 时 app deploy / app rollback 的行为与 5b 之前
// 逐字节一致。批量必须显式要求——「我给了同样的命令，为什么这次发了五台」是这类工具
// 最不该出现的意外。
type batchFlags struct {
	hosts        string
	artifact     string
	batchID      string
	batchSize    int
	concurrency  int
	pause        time.Duration
	allowPartial bool
	keepGoing    bool
}

func (f *batchFlags) bind(cmd *cobra.Command) {
	// 刻意不叫 --to：rollback 的 --to 已经是「回滚到哪个版本」，一个旗标两种含义
	// 会让 `--to 1.0.0` 与 `--to web-1` 长得一模一样，而它们要做的事完全不同。
	cmd.Flags().StringVar(&f.hosts, "hosts", "",
		"批量目标：逗号分隔的主机名，或 @<应用名>（用那台机上登记的部署目标）。不给就是单机操作")
	cmd.Flags().StringVar(&f.artifact, "artifact", "",
		"本地制品文件：准备阶段用它补齐缺这个制品的机器（批量部署专用）")
	cmd.Flags().StringVar(&f.batchID, "batch", "",
		"批次号。同名重跑会跳过已完成的机器（批次号就是每台机上的幂等键）；不给则自动生成一个")
	cmd.Flags().IntVar(&f.batchSize, "batch-size", 0, "一波几台主机（默认全部）；波与波之间串行")
	cmd.Flags().IntVar(&f.concurrency, "concurrency", 0, "一波内同时几台（默认等于 batch-size）")
	cmd.Flags().DurationVar(&f.pause, "pause", 0, "一波结束后到下一波之前的等待")
	cmd.Flags().BoolVar(&f.allowPartial, "allow-partial", false,
		"准备阶段有主机没过时仍然继续（那几台会被标成 skipped）；默认拒绝开始")
	cmd.Flags().BoolVar(&f.keepGoing, "keep-going", false,
		"某一波有失败时仍然继续下一波；默认失败即停")
}

// enabled 表示这次调用是不是批量模式。只有一个判据：给没给 --hosts。
func (f *batchFlags) enabled() bool { return strings.TrimSpace(f.hosts) != "" }

// runBatch 组装并跑一个批次。
func runBatch(ctx context.Context, opts *rootOptions, flags *batchFlags, req fleet.Request) error {
	names, fromRegistry, err := parseTargetFlag(ctx, opts, flags.hosts)
	if err != nil {
		return err
	}
	targets, err := resolveTargets(ctx, opts, names)
	if err != nil {
		return err
	}

	batchID := flags.batchID
	if batchID == "" {
		batchID = idgen.New("batch")
	}

	req.Targets = targets
	req.BatchID = batchID
	req.BatchSize = flags.batchSize
	req.Concurrency = flags.concurrency
	req.Pause = flags.pause
	req.AllowPartial = flags.allowPartial
	req.KeepGoing = flags.keepGoing

	if opts.json {
		// 进度走 stderr：--json 的 stdout 必须只有一份能直接被 jq 吃下的 JSON。
		req.Logf = func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
	} else {
		req.Logf = func(format string, args ...any) { fmt.Fprintf(os.Stdout, format+"\n", args...) }
		fmt.Printf("批次 %s：%s %s → %d 台主机", batchID, req.Application, batchActionLabel(req.Action), len(targets))
		if fromRegistry {
			fmt.Print("（目标来自登记的部署目标）")
		}
		fmt.Println()
		fmt.Printf("目标：%s\n", strings.Join(names, " "))
		fmt.Printf("批次大小 %d，并发 %d", effectiveBatchSize(flags, len(targets)), effectiveConcurrency(flags, len(targets)))
		if flags.pause > 0 {
			fmt.Printf("，批间隔 %s", flags.pause)
		}
		fmt.Println()
	}

	report, runErr := fleet.Run(ctx, req)

	if opts.json {
		if err := opts.printJSON(report); err != nil {
			return err
		}
	} else {
		printBatchReport(report)
	}
	if runErr != nil {
		return runErr
	}
	if !opts.json {
		fmt.Printf("\n要重跑这个批次（已完成的机器不会被重做）：opsctl app %s ... --hosts %s --batch %s\n",
			string(report.Action), flags.hosts, batchID)
	}
	return nil
}

func effectiveBatchSize(flags *batchFlags, total int) int {
	if flags.batchSize > 0 {
		return flags.batchSize
	}
	return total
}

func effectiveConcurrency(flags *batchFlags, total int) int {
	size := effectiveBatchSize(flags, total)
	if flags.concurrency > 0 && flags.concurrency < size {
		return flags.concurrency
	}
	return size
}

func batchActionLabel(action fleet.Action) string {
	if action == fleet.ActionRollback {
		return "回滚"
	}
	return "部署"
}

func printBatchReport(report *fleet.Report) {
	fmt.Println("\n结果：")
	for _, result := range report.Results {
		line := fmt.Sprintf("  %-16s %s", result.Host, result.Status)
		if result.Version != "" {
			line += "  版本 " + result.Version
		}
		if result.Slot != "" {
			line += "  槽位 " + result.Slot
		}
		if result.OperationID != "" {
			line += "  " + result.OperationID
		}
		fmt.Println(line)
		switch {
		case result.Status == fleet.StatusFailed:
			fmt.Printf("                   %s：%s\n", result.ErrorCode, result.ErrorMessage)
		case result.Detail != "":
			fmt.Printf("                   %s\n", result.Detail)
		}
	}
	fmt.Printf("\n汇总（用时 %s）：%s\n", report.Duration.Round(time.Millisecond), report.Summary())
}

// resolveTargets 把主机名解析成「客户端 + 地址」。
//
// 地址从**当前目标**（--host 或 socket 指定的那台）的主机表里读——与 5a 的 --host
// 是同一条规则。地址为空表示本机，那一条就走当前的 socket 客户端。
func resolveTargets(ctx context.Context, opts *rootOptions, names []string) ([]fleet.Target, error) {
	targets := make([]fleet.Target, 0, len(names))
	for _, name := range names {
		host, err := opts.client().GetHost(ctx, name)
		if err != nil {
			return nil, err
		}
		c, err := opts.clientForKey(host.Address)
		if err != nil {
			return nil, err
		}
		targets = append(targets, fleet.Target{
			Name:    host.Name,
			Address: host.Address,
			Client:  hostClient{client: c},
		})
	}
	return targets, nil
}

// hostClient 把协议客户端适配成 fleet 需要的那一组动作。
//
// 适配层刻意放在这里而不是让 fleet 直接依赖 client：批次逻辑要在没有真实 opsd 的
// 情况下被完整测到，而这一层是唯一需要真实网络的地方。
type hostClient struct {
	client *client.Client
}

func (h hostClient) Identity(ctx context.Context) (*v1.IdentityResponse, error) {
	return h.client.Identity(ctx)
}

func (h hostClient) GetApplication(ctx context.Context, ref string) (*v1.Application, error) {
	return h.client.GetApplication(ctx, ref)
}

func (h hostClient) GetArtifact(ctx context.Context, ref string) (*v1.Artifact, error) {
	return h.client.GetArtifact(ctx, ref)
}

func (h hostClient) ListReleases(ctx context.Context, app string, limit int) (*v1.ReleaseListResponse, error) {
	return h.client.ListReleases(ctx, app, limit)
}

func (h hostClient) Deploy(ctx context.Context, app, manifest, idempotencyKey, createdBy string) (*v1.DeployResponse, error) {
	return h.client.DeployApplication(ctx, app, manifest, client.ActionInput{
		IdempotencyKey: idempotencyKey,
		CreatedBy:      createdBy,
	})
}

func (h hostClient) Rollback(ctx context.Context, app, to, idempotencyKey, createdBy string) (*v1.DeployResponse, error) {
	return h.client.RollbackApplication(ctx, app, to, client.ActionInput{
		IdempotencyKey: idempotencyKey,
		CreatedBy:      createdBy,
	})
}

func (h hostClient) GetOperation(ctx context.Context, id string) (*v1.Operation, error) {
	return h.client.GetOperation(ctx, id)
}

func (h hostClient) UploadArtifact(ctx context.Context, path, createdBy string) (*v1.Artifact, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false, domain.NewError(v1.CodeInvalidRequest, "无法读取制品文件 %q：%v", path, err)
	}
	defer file.Close()

	return h.client.UploadArtifact(ctx, client.UploadArtifactInput{
		Name:      filepath.Base(path),
		MediaType: "application/octet-stream",
		Body:      file,
		CreatedBy: createdBy,
	})
}

// verifyLocalArtifact 在批量部署开始之前，确认本地文件的内容就是 manifest 里那个摘要。
//
// 这一条必须在**任何一次上传之前**做：不然「手工打包的 tar 与 manifest 对不上」会以
// 「五台机都装上了错的制品」收场，而那时没有任何一处报错——每台机上的部署都会成功，
// 因为它们的制品校验对着的是自己刚收到的那份字节。
func verifyLocalArtifact(path, want string) error {
	file, err := os.Open(path)
	if err != nil {
		return domain.NewError(v1.CodeInvalidRequest, "无法读取制品文件 %q：%v", path, err)
	}
	defer file.Close()

	digest, size, err := domain.DigestOf(file)
	if err != nil {
		return domain.NewError(v1.CodeInvalidRequest, "无法计算 %q 的摘要：%v", path, err)
	}
	expected, err := domain.ParseDigest(want)
	if err != nil {
		return err
	}
	if digest != expected {
		return domain.NewError(v1.CodeArtifactChecksum,
			"本地文件 %q（%d 字节）的摘要是 %s，与 manifest 里写的 %s 不一致：先确认打包的是哪一个",
			path, size, digest, expected)
	}
	return nil
}

// rejectIncompatibleBatchFlags 拒绝那些在批量模式下**没有意义**的旗标。
//
// 静默忽略它们是最糟的选择：运维给了 --retry-max 5，就会以为失败的机器会自己重试，
// 而批次其实一次都不重试。
func rejectIncompatibleBatchFlags(cmd *cobra.Command, flags *actionFlags) error {
	if cmd.Flags().Changed("idempotency-key") {
		return domain.NewError(v1.CodeInvalidRequest,
			"批量模式下不能给 --idempotency-key：耦合键要按批次号+应用+动作拼出来，"+
				"否则同名批次重跑会得到 IDEMPOTENCY_CONFLICT，而那个报错指不出真正的原因")
	}
	for _, name := range []string{"retry-max", "retry-base", "retry-max-delay"} {
		if cmd.Flags().Changed(name) {
			return domain.NewError(v1.CodeInvalidRequest,
				"批量模式下不能给 --%s：失败的机器不会自动重试（自动重试等于把同一个坏版本再推一次）。"+
					"要重试某一台请对它跑 operation retry；要整批重来请换一个批次号", name)
		}
	}
	_ = flags
	return nil
}
