package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"

	"github.com/spf13/cobra"
)

func newSpecCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "spec",
		Short: "提交与查看应用的 manifest（应用当前规格）",
	}
	cmd.AddCommand(newSpecPutCommand(opts), newSpecShowCommand(opts))
	return cmd
}

func newSpecPutCommand(opts *rootOptions) *cobra.Command {
	var (
		app       string
		file      string
		updatedBy string
	)

	cmd := &cobra.Command{
		Use:   "put --app <name> --file <manifest.yaml>",
		Short: "提交应用的 manifest，替换上一次提交的规格",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if app == "" || file == "" {
				return domain.NewError(v1.CodeInvalidRequest, "必须同时提供 --app 与 --file")
			}
			// manifest 原样交给 opsd 解码：本地解一遍再发结构化数据会让
			// 「CLI 认为合法、opsd 认为非法」成为可能。
			manifest, err := os.ReadFile(file)
			if err != nil {
				return domain.NewError(v1.CodeInvalidRequest, "无法读取 %q: %v", file, err)
			}

			spec, err := opts.client().PutApplicationSpec(cmd.Context(), app, manifest, updatedBy)
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(spec)
			}
			printSpec(spec)
			return nil
		},
	}

	cmd.Flags().StringVar(&app, "app", "", "应用名称或 ID")
	cmd.Flags().StringVar(&file, "file", "", "manifest 文件路径（YAML）")
	cmd.Flags().StringVar(&updatedBy, "updated-by", "", "调用方标识")

	return cmd
}

func newSpecShowCommand(opts *rootOptions) *cobra.Command {
	var app string

	cmd := &cobra.Command{
		Use:   "show --app <name>",
		Short: "查看应用当前的 manifest",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if app == "" {
				return domain.NewError(v1.CodeInvalidRequest, "必须提供 --app")
			}
			spec, err := opts.client().GetApplicationSpec(cmd.Context(), app)
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(spec)
			}
			printSpec(spec)
			return nil
		},
	}

	cmd.Flags().StringVar(&app, "app", "", "应用名称或 ID")
	return cmd
}

// printSpec 的字段名与 manifest 的键一致（见 AGENTS.md 的语言约定：API 字段名不翻译）。
// 环境变量按键排序输出，否则每次运行的顺序都不同，diff 起来没有意义。
func printSpec(spec *v1.ApplicationSpec) {
	fmt.Printf("application: %s\n", spec.Application)
	fmt.Printf("apiVersion:  %s\n", spec.APIVersion)
	fmt.Printf("kind:        %s\n", spec.Kind)
	fmt.Printf("runtime:     %s\n", spec.Runtime)
	fmt.Printf("artifact:    %s\n", artifactRefOf(spec.Artifact))
	fmt.Printf("unpack:      %s", spec.Artifact.Unpack.Strategy)
	if spec.Artifact.Unpack.StripComponents > 0 {
		fmt.Printf("（stripComponents=%d）", spec.Artifact.Unpack.StripComponents)
	}
	fmt.Println()
	fmt.Printf("argv:        %s\n", strings.Join(spec.Exec.Argv, " "))
	fmt.Printf("workingDir:  %s\n", spec.Exec.WorkingDirectory)
	fmt.Printf("runUser:     %s\n", spec.Exec.RunUser)
	for _, name := range sortedKeys(spec.Exec.Environment) {
		fmt.Printf("environment: %s=%s\n", name, spec.Exec.Environment[name])
	}
	for _, name := range sortedKeys(spec.Exec.SecretEnvironment) {
		// 只打印凭据的引用与交付方式，永远不打印值。
		ref := spec.Exec.SecretEnvironment[name]
		fmt.Printf("secret:      %s（kind=%s, name=%s）\n", name, ref.Kind, ref.Name)
	}
	if len(spec.Exec.Ports) > 0 {
		ports := make([]string, 0, len(spec.Exec.Ports))
		for _, port := range spec.Exec.Ports {
			ports = append(ports, fmt.Sprintf("%d", port))
		}
		fmt.Printf("ports:       %s\n", strings.Join(ports, ","))
	}
	fmt.Printf("readiness:   %s %s\n", spec.Health.Readiness.Type, spec.Health.Readiness.Target)
	fmt.Printf("logs:        %s\n", spec.Logs.Directory)
	fmt.Printf("unit:        %s\n", spec.Systemd.UnitName)
}

func artifactRefOf(artifact v1.SpecArtifact) string {
	if artifact.ID != "" {
		return artifact.ID
	}
	return artifact.Digest
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
