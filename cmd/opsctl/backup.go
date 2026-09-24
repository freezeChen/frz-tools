package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/client"
	"github.com/freezeChen/frz-tools/internal/domain"

	"github.com/spf13/cobra"
)

func newBackupCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "管理备份策略，触发、校验与恢复备份",
		Long: "备份策略声明「备份什么、怎么编码、保留多久」。\n\n" +
			"backup run / verify / restore 走 opsd 的 Operation（kind=backup.run 等），\n" +
			"因此可以用 operation get/logs/cancel/retry 查询进度、看日志、取消与重试。\n" +
			"原地恢复会覆盖真实数据，必须显式加 --confirm。",
	}
	cmd.AddCommand(
		newBackupPolicyCommand(opts),
		newBackupRunCommand(opts),
		newBackupListCommand(opts),
		newBackupShowCommand(opts),
		newBackupVerifyCommand(opts),
		newBackupRestoreCommand(opts),
	)
	return cmd
}

func newBackupPolicyCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "policy",
		Short: "提交、查看备份策略",
	}
	cmd.AddCommand(
		newBackupPolicyPutCommand(opts),
		newBackupPolicyListCommand(opts),
		newBackupPolicyGetCommand(opts),
	)
	return cmd
}

func newBackupPolicyPutCommand(opts *rootOptions) *cobra.Command {
	var file string

	cmd := &cobra.Command{
		Use:   "put --file <manifest.yaml>",
		Short: "提交或覆盖一份备份策略（kind: BackupPolicy）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if file == "" {
				return domain.NewError(v1.CodeInvalidRequest, "必须提供 --file")
			}
			raw, err := os.ReadFile(file)
			if err != nil {
				return domain.NewError(v1.CodeInvalidRequest, "读取 %s 失败: %v", file, err)
			}

			policy, err := opts.client().PutBackupPolicy(cmd.Context(), string(raw), "")
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(policy)
			}
			fmt.Printf("备份策略 %s 已提交（kind=%s）\n", policy.Name, policy.Resource.Kind)
			return nil
		},
	}

	cmd.Flags().StringVar(&file, "file", "", "策略 manifest 文件路径（必填）")
	return cmd
}

func newBackupPolicyListCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "列出全部备份策略",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := opts.client().ListBackupPolicies(cmd.Context())
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(result)
			}
			if len(result.Items) == 0 {
				fmt.Println("没有备份策略")
				return nil
			}
			for _, policy := range result.Items {
				fmt.Printf("%s\t%s\t加密=%t\t保留=%s\n",
					policy.Name, policy.Resource.Kind, policy.Encoding.Encryption.Enabled,
					describeRetention(policy.Retention))
			}
			return nil
		},
	}
	return cmd
}

func newBackupPolicyGetCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "查看一份备份策略",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			policy, err := opts.client().GetBackupPolicy(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(policy)
			}
			printBackupPolicy(policy)
			return nil
		},
	}
	return cmd
}

func newBackupRunCommand(opts *rootOptions) *cobra.Command {
	flags := &backupActionFlags{}

	cmd := &cobra.Command{
		Use:   "run --policy <name>",
		Short: "触发一次备份（返回 Operation，异步执行）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if flags.policy == "" {
				return domain.NewError(v1.CodeInvalidRequest, "必须提供 --policy")
			}
			in, err := flags.input(cmd)
			if err != nil {
				return err
			}
			op, created, err := opts.client().RunBackup(cmd.Context(), flags.policy, in)
			if err != nil {
				return err
			}
			return reportOperation(opts, op, created)
		},
	}

	cmd.Flags().StringVar(&flags.policy, "policy", "", "备份策略名（必填）")
	flags.bind(cmd)
	return cmd
}

func newBackupListCommand(opts *rootOptions) *cobra.Command {
	var (
		policy string
		limit  int
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "列出备份记录（默认最近 100 条）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := opts.client().ListBackups(cmd.Context(), policy, limit)
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(result)
			}
			if len(result.Items) == 0 {
				fmt.Println("没有备份记录")
				return nil
			}
			for _, backup := range result.Items {
				fmt.Printf("%s\t%s\t%s\t%s\t%d 字节\n",
					backup.ID, backup.Status, backup.ResourceKind,
					backup.StartedAt.Format(time.RFC3339), backup.StoredBytes)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&policy, "policy", "", "只看某份策略下的备份")
	cmd.Flags().IntVar(&limit, "limit", 0, "返回条数上限（默认 100）")
	return cmd
}

func newBackupShowCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "查看一条备份记录的详情",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			backup, err := opts.client().GetBackup(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(backup)
			}
			printBackup(backup)
			return nil
		},
	}
	return cmd
}

func newBackupVerifyCommand(opts *rootOptions) *cobra.Command {
	flags := &backupActionFlags{}

	cmd := &cobra.Command{
		Use:   "verify <id>",
		Short: "校验一份备份的流是否自洽（不接触目标资源）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in, err := flags.input(cmd)
			if err != nil {
				return err
			}
			op, created, err := opts.client().VerifyBackup(cmd.Context(), args[0], in)
			if err != nil {
				return err
			}
			return reportOperation(opts, op, created)
		},
	}

	flags.bind(cmd)
	return cmd
}

func newBackupRestoreCommand(opts *rootOptions) *cobra.Command {
	flags := &backupActionFlags{}
	var (
		mode    string
		confirm bool
	)

	cmd := &cobra.Command{
		Use:   "restore <id> --mode isolated|inPlace [--confirm]",
		Short: "恢复一份备份（默认只允许隔离恢复）",
		Long: "isolated：恢复到临时目录/临时实例，完成后销毁，**不碰真实数据**。\n" +
			"inPlace：恢复到真实目标——这是本工具破坏性最强的动作，必须加 --confirm。",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in, err := flags.input(cmd)
			if err != nil {
				return err
			}
			op, created, err := opts.client().RestoreBackup(cmd.Context(), args[0], mode, confirm, in)
			if err != nil {
				return err
			}
			return reportOperation(opts, op, created)
		},
	}

	cmd.Flags().StringVar(&mode, "mode", string(domain.RestoreIsolated),
		"恢复模式：isolated（隔离，默认）或 inPlace（覆盖真实数据）")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "确认原地恢复（inPlace 必填）")
	flags.bind(cmd)
	return cmd
}

// backupActionFlags 是备份类操作共用的参数。
type backupActionFlags struct {
	policy         string
	idempotencyKey string
	createdBy      string
	retryMax       int
	retryBase      time.Duration
	retryMaxDelay  time.Duration
}

func (f *backupActionFlags) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.idempotencyKey, "idempotency-key", "", "幂等键：相同请求重复提交会返回同一个操作")
	cmd.Flags().StringVar(&f.createdBy, "created-by", "", "调用方标识")
	cmd.Flags().IntVar(&f.retryMax, "retry-max", 0,
		"最多尝试次数，含首次（1 表示不重试，上限 10）。省略则不自动重试")
	cmd.Flags().DurationVar(&f.retryBase, "retry-base", 0, "退避基数（默认 5s）")
	cmd.Flags().DurationVar(&f.retryMaxDelay, "retry-max-delay", 0, "退避上限（默认 5m）")
}

// input 组装公共参数。重试参数与 operation submit 用同一套语义：只有显式设置
// --retry-* 才构成策略。
func (f *backupActionFlags) input(cmd *cobra.Command) (client.BackupActionInput, error) {
	in := client.BackupActionInput{
		IdempotencyKey: f.idempotencyKey,
		CreatedBy:      f.createdBy,
	}
	if !cmd.Flags().Changed("retry-max") && !cmd.Flags().Changed("retry-base") && !cmd.Flags().Changed("retry-max-delay") {
		return in, nil
	}
	base, err := wholeSeconds(f.retryBase, "retry-base")
	if err != nil {
		return in, err
	}
	maxDelay, err := wholeSeconds(f.retryMaxDelay, "retry-max-delay")
	if err != nil {
		return in, err
	}
	in.Retry = &v1.RetrySpec{
		MaxAttempts:      f.retryMax,
		BaseDelaySeconds: base,
		MaxDelaySeconds:  maxDelay,
	}
	return in, nil
}

// reportOperation 打印一个异步操作的提交结果。备份与 runtime 一样是异步的：
// 这里只负责提交，进度用 operation 命令查。
func reportOperation(opts *rootOptions, op *v1.Operation, created bool) error {
	if opts.json {
		return opts.printJSON(op)
	}
	if !created {
		fmt.Println("相同请求已存在，复用既有操作")
	}
	printOperation(op)
	fmt.Printf("\n查询进度：opsctl operation get %s\n查看日志：opsctl operation logs %s\n", op.ID, op.ID)
	return nil
}

func printBackupPolicy(policy *v1.BackupPolicy) {
	fmt.Printf("id:          %s\n", policy.ID)
	fmt.Printf("name:        %s\n", policy.Name)
	fmt.Printf("kind:        %s\n", policy.Resource.Kind)
	for _, p := range policy.Resource.Paths {
		fmt.Printf("path:        %s\n", p)
	}
	for _, pattern := range policy.Resource.Exclude {
		fmt.Printf("exclude:     %s\n", pattern)
	}
	if policy.Resource.Symlinks != "" {
		fmt.Printf("symlinks:    %s\n", policy.Resource.Symlinks)
	}
	fmt.Printf("compression: %s\n", policy.Encoding.Compression)
	fmt.Printf("encryption:  %t\n", policy.Encoding.Encryption.Enabled)
	if ref := policy.Encoding.Encryption.KeySecret; ref != nil {
		// 只打印引用，不打印密钥本身。
		fmt.Printf("keySecret:   %s:%s\n", ref.Kind, ref.Name)
	}
	fmt.Printf("retention:   %s\n", describeRetention(policy.Retention))
	fmt.Printf("timeout:     backup=%ds restore=%ds\n",
		policy.Timeout.BackupSeconds, policy.Timeout.RestoreSeconds)
}

func printBackup(backup *v1.Backup) {
	fmt.Printf("id:          %s\n", backup.ID)
	fmt.Printf("policyId:    %s\n", backup.PolicyID)
	if backup.OperationID != "" {
		fmt.Printf("operationId: %s\n", backup.OperationID)
	}
	fmt.Printf("status:      %s\n", backup.Status)
	fmt.Printf("kind:        %s\n", backup.ResourceKind)
	if backup.StorageDigest != "" {
		fmt.Printf("digest:      %s\n", backup.StorageDigest)
	}
	fmt.Printf("logicalBytes:%d\n", backup.LogicalBytes)
	fmt.Printf("storedBytes: %d\n", backup.StoredBytes)
	if backup.Compression != "" {
		fmt.Printf("compression: %s\n", backup.Compression)
	}
	if backup.EncryptionKeyID != "" {
		fmt.Printf("encryption:  keyId=%s\n", backup.EncryptionKeyID)
	}
	if backup.Tool != "" {
		fmt.Printf("tool:        %s\n", backup.Tool)
	}
	// 校验状态三态要分清楚：「通过」「不通过」与「从没校验过」是三件不同的事。
	switch {
	case backup.VerifiedOK == nil:
		fmt.Println("verified:    尚未校验")
	case *backup.VerifiedOK:
		fmt.Printf("verified:    通过（%s）\n", formatTimePtr(backup.VerifiedAt))
	default:
		fmt.Printf("verified:    **不通过**（%s）\n", formatTimePtr(backup.VerifiedAt))
	}
	fmt.Printf("startedAt:   %s\n", backup.StartedAt.Format(time.RFC3339))
	if backup.FinishedAt != nil {
		fmt.Printf("finishedAt:  %s\n", backup.FinishedAt.Format(time.RFC3339))
	}
	if backup.ErrorCode != "" {
		fmt.Printf("error:       %s (%s)\n", backup.ErrorCode, backup.ErrorMessage)
	}
}

func describeRetention(retention v1.BackupRetention) string {
	var parts []string
	if retention.KeepLast > 0 {
		parts = append(parts, "keepLast="+strconv.Itoa(retention.KeepLast))
	}
	if retention.KeepDays > 0 {
		parts = append(parts, "keepDays="+strconv.Itoa(retention.KeepDays))
	}
	if retention.GFS.Daily > 0 {
		parts = append(parts, "daily="+strconv.Itoa(retention.GFS.Daily))
	}
	if retention.GFS.Weekly > 0 {
		parts = append(parts, "weekly="+strconv.Itoa(retention.GFS.Weekly))
	}
	if retention.GFS.Monthly > 0 {
		parts = append(parts, "monthly="+strconv.Itoa(retention.GFS.Monthly))
	}
	if len(parts) == 0 {
		return "（未声明）"
	}
	return strings.Join(parts, " ")
}

func formatTimePtr(t *time.Time) string {
	if t == nil {
		return "时间未知"
	}
	return t.Format(time.RFC3339)
}
