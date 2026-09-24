package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/client"
	"github.com/freezeChen/frz-tools/internal/domain"

	"github.com/spf13/cobra"
)

const defaultArtifactMediaType = "application/octet-stream"

func newArtifactCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "artifact",
		Short: "上传、查询、校验与清理制品",
	}
	cmd.AddCommand(
		newArtifactPutCommand(opts),
		newArtifactListCommand(opts),
		newArtifactInspectCommand(opts),
		newArtifactDownloadCommand(opts),
		newArtifactVerifyCommand(opts),
		newArtifactDeleteCommand(opts),
		newArtifactGCCommand(opts),
	)
	return cmd
}

func newArtifactPutCommand(opts *rootOptions) *cobra.Command {
	var (
		name      string
		mediaType string
		digest    string
		createdBy string
	)

	cmd := &cobra.Command{
		Use:   "put <path>",
		Short: "上传一个制品文件",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			file, err := os.Open(args[0])
			if err != nil {
				return domain.NewError(v1.CodeInvalidRequest, "无法读取 %q: %v", args[0], err)
			}
			defer file.Close()

			if mediaType == "" {
				mediaType = defaultArtifactMediaType
			}
			if name == "" {
				name = filepath.Base(args[0])
			}

			artifact, created, err := opts.client().UploadArtifact(cmd.Context(), client.UploadArtifactInput{
				Name:      name,
				MediaType: mediaType,
				Digest:    digest,
				Body:      file,
				CreatedBy: createdBy,
			})
			if err != nil {
				return err
			}

			if opts.json {
				return opts.printJSON(artifact)
			}
			if !created {
				fmt.Println("相同内容已存在，复用既有制品")
			}
			printArtifact(artifact)
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "制品展示名，仅作元数据（默认取文件名）")
	cmd.Flags().StringVar(&mediaType, "media-type", "", "制品 MIME 类型")
	cmd.Flags().StringVar(&digest, "sha256", "", "期望的 sha256:<hex>，用于上传前声明校验")
	cmd.Flags().StringVar(&createdBy, "created-by", "", "调用方标识")

	return cmd
}

func newArtifactListCommand(opts *rootOptions) *cobra.Command {
	var (
		cursor string
		limit  int
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "列出制品",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			response, err := opts.client().ListArtifacts(cmd.Context(), cursor, limit)
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(response)
			}
			for i := range response.Items {
				printArtifact(&response.Items[i])
				fmt.Println()
			}
			if response.NextCursor != "" {
				fmt.Printf("下一页：--cursor %s\n", response.NextCursor)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&cursor, "cursor", "", "从该制品 ID 之后继续分页")
	cmd.Flags().IntVar(&limit, "limit", 0, "返回条数上限")
	return cmd
}

func newArtifactInspectCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <artifact-id|digest>",
		Short: "查看单个制品",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			artifact, err := opts.client().GetArtifact(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(artifact)
			}
			printArtifact(artifact)
			return nil
		},
	}
}

func newArtifactDownloadCommand(opts *rootOptions) *cobra.Command {
	var output string

	cmd := &cobra.Command{
		Use:   "download <artifact-id|digest> --output <path>",
		Short: "下载制品内容到本地文件",
		Long: "下载制品内容到本地文件。内容按存储的摘要校验后才落盘，因此下载失败或\n" +
			"内容与记录不一致时，--output 指定的路径不会被写入半截内容。",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output == "" {
				return domain.NewError(v1.CodeInvalidRequest, "必须提供 --output")
			}
			artifact, err := opts.client().DownloadArtifact(cmd.Context(), args[0], output)
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(artifact)
			}
			fmt.Printf("已下载 %s\n", output)
			fmt.Printf("digest: %s\n", artifact.Digest)
			fmt.Printf("size:   %d\n", artifact.Size)
			return nil
		},
	}

	cmd.Flags().StringVar(&output, "output", "", "输出文件路径")
	return cmd
}

func newArtifactVerifyCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "verify <artifact-id|digest>",
		Short: "从存储重算摘要并核对记录",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := opts.client().VerifyArtifact(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(result)
			}
			fmt.Printf("已校验 %s\n摘要 %s\n", result.ArtifactID, result.Digest)
			return nil
		},
	}
}

func newArtifactDeleteCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <artifact-id|digest>",
		Short: "删除制品；被 Release 引用时会被拒绝",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := opts.client().DeleteArtifact(cmd.Context(), args[0]); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "已删除 %s\n", args[0]); err != nil {
				return err
			}
			return nil
		},
	}
}

func newArtifactGCCommand(opts *rootOptions) *cobra.Command {
	var (
		keepLast  int
		olderThan time.Duration
		dryRun    bool
	)

	cmd := &cobra.Command{
		Use:   "gc",
		Short: "清理超出保留策略且未被引用的制品",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := opts.client().CollectArtifacts(cmd.Context(), client.CollectArtifactsInput{
				KeepLast:    keepLast,
				OlderThanMs: olderThan.Milliseconds(),
				DryRun:      dryRun,
			})
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(result)
			}
			mode := "已清理"
			if result.DryRun {
				mode = "预演（未删除）"
			}
			fmt.Printf("%s：移除 %d 个制品，回收孤儿内容 %d 个，保留 %d 个，释放 %d 字节\n",
				mode, len(result.Removed), len(result.Orphans), result.Kept, result.FreedBytes)
			for _, id := range result.Removed {
				fmt.Printf("  - %s\n", id)
			}
			for _, digest := range result.Orphans {
				fmt.Printf("  ? %s（无元数据）\n", digest)
			}
			return nil
		},
	}

	cmd.Flags().IntVar(&keepLast, "keep", 0, "保留最近 N 个制品")
	cmd.Flags().DurationVar(&olderThan, "older-than", 0, "仅清理早于该时长的制品")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "只报告将要删除的内容")
	return cmd
}

func printArtifact(artifact *v1.Artifact) {
	fmt.Printf("id:        %s\n", artifact.ID)
	fmt.Printf("digest:    %s\n", artifact.Digest)
	fmt.Printf("size:      %s\n", strconv.FormatInt(artifact.Size, 10))
	if artifact.Name != "" {
		fmt.Printf("name:      %s\n", artifact.Name)
	}
	if artifact.MediaType != "" {
		fmt.Printf("mediaType: %s\n", artifact.MediaType)
	}
	fmt.Printf("createdAt: %s\n", artifact.CreatedAt.Format(time.RFC3339))
}
