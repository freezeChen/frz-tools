package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	v1 "frz-tools/api/v1"
	"frz-tools/internal/adapters/client"
	"frz-tools/internal/domain"

	"github.com/spf13/cobra"
)

type rootOptions struct {
	socket  string
	timeout time.Duration
	json    bool
}

func main() {
	root := newRootCommand()
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "opsctl:", domain.MessageOf(err))
		os.Exit(v1.ExitCode(domain.CodeOf(err)))
	}
}

func newRootCommand() *cobra.Command {
	opts := &rootOptions{}

	root := &cobra.Command{
		Use:           "opsctl",
		Short:         "opsctl submits and inspects operations handled by opsd",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVar(&opts.socket, "socket", defaultSocket(), "path to the opsd unix socket (env OPSD_SOCKET)")
	root.PersistentFlags().DurationVar(&opts.timeout, "timeout", 30*time.Second, "HTTP client timeout")
	root.PersistentFlags().BoolVar(&opts.json, "json", false, "print raw JSON instead of human-readable text")

	root.AddCommand(
		newHealthCommand(opts),
		newOperationCommand(opts),
		newArtifactCommand(opts),
		newAppCommand(opts),
		newReleaseCommand(opts),
		newConfigCommand(opts),
	)
	return root
}

func defaultSocket() string {
	if fromEnv := os.Getenv("OPSD_SOCKET"); fromEnv != "" {
		return fromEnv
	}
	return "/run/opsd/opsd.sock"
}

func (o *rootOptions) client() *client.Client {
	return client.New(o.socket, o.timeout)
}

func (o *rootOptions) printJSON(value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	return nil
}

func printOperation(op *v1.Operation) {
	fmt.Printf("id:        %s\n", op.ID)
	fmt.Printf("kind:      %s\n", op.Kind)
	fmt.Printf("resource:  %s\n", op.Resource)
	fmt.Printf("status:    %s\n", op.Status)
	if op.Phase != "" {
		fmt.Printf("phase:     %s\n", op.Phase)
	}
	fmt.Printf("dryRun:    %t\n", op.DryRun)
	if op.RetryOf != "" {
		fmt.Printf("retryOf:   %s\n", op.RetryOf)
	}
	if op.ExitCode != nil {
		fmt.Printf("exitCode:  %d\n", *op.ExitCode)
	}
	if op.ErrorCode != "" {
		fmt.Printf("error:     %s (%s)\n", op.ErrorCode, op.ErrorMessage)
	}
	fmt.Printf("createdAt: %s\n", op.CreatedAt.Format(time.RFC3339))
	if op.FinishedAt != nil {
		fmt.Printf("finishedAt:%s\n", op.FinishedAt.Format(time.RFC3339))
	}
}
