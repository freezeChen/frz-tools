package main

import (
	"encoding/json"
	"fmt"
	"time"

	v1 "frz-tools/api/v1"
	"frz-tools/internal/domain"

	"github.com/spf13/cobra"
)

func newOperationCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "operation",
		Short: "Submit, inspect, cancel, retry and read logs of operations",
	}
	cmd.AddCommand(
		newSubmitCommand(opts),
		newGetCommand(opts),
		newCancelCommand(opts),
		newRetryCommand(opts),
		newLogsCommand(opts),
	)
	return cmd
}

func newSubmitCommand(opts *rootOptions) *cobra.Command {
	var (
		kind     string
		resource string
		idemKey  string
		dryRun   bool
	)

	cmd := &cobra.Command{
		Use:   "submit --kind <kind> --resource <name> [--dry-run] -- <argv...>",
		Short: "Submit an operation for execution by opsd",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.ArgsLenAtDash() == -1 {
				return domain.NewError(v1.CodeInvalidRequest, "pass the command argv after --, for example: -- /usr/bin/true")
			}
			if len(args) == 0 {
				return domain.NewError(v1.CodeInvalidRequest, "argv after -- must not be empty")
			}

			spec, err := json.Marshal(v1.ExecutorCommandSpec{Argv: args})
			if err != nil {
				return err
			}

			operation, created, err := opts.client().CreateOperation(cmd.Context(), v1.CreateOperationRequest{
				Kind:           kind,
				Resource:       resource,
				DryRun:         dryRun,
				Spec:           spec,
				IdempotencyKey: idemKey,
			})
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(operation)
			}
			if !created {
				fmt.Println("reused existing operation for this idempotency key")
			}
			printOperation(operation)
			return nil
		},
	}

	cmd.Flags().StringVar(&kind, "kind", v1.KindExecutorCommand, "operation kind")
	cmd.Flags().StringVar(&resource, "resource", "", "resource to lock for the duration of the operation (required)")
	cmd.Flags().StringVar(&idemKey, "idempotency-key", "", "idempotency key; repeating it with the same request reuses the operation")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "validate and record the plan without executing anything")
	_ = cmd.MarkFlagRequired("resource")

	return cmd
}

func newGetCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "get <operation-id>",
		Short: "Show one operation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			operation, err := opts.client().GetOperation(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(operation)
			}
			printOperation(operation)
			return nil
		},
	}
}

func newCancelCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <operation-id>",
		Short: "Cancel a pending or running operation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			operation, err := opts.client().CancelOperation(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(operation)
			}
			printOperation(operation)
			return nil
		},
	}
}

func newRetryCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "retry <operation-id>",
		Short: "Retry a failed or cancelled operation as a new operation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			operation, err := opts.client().RetryOperation(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(operation)
			}
			printOperation(operation)
			return nil
		},
	}
}

func newLogsCommand(opts *rootOptions) *cobra.Command {
	var (
		cursor int64
		limit  int
		follow bool
	)

	cmd := &cobra.Command{
		Use:   "logs <operation-id>",
		Short: "Print the structured log of an operation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if follow {
				return domain.NewError(v1.CodeInvalidRequest, "--follow is not implemented in this iteration; poll with --cursor instead")
			}
			response, err := opts.client().Logs(cmd.Context(), args[0], cursor, limit)
			if err != nil {
				return err
			}
			if opts.json {
				return opts.printJSON(response)
			}
			for _, entry := range response.Items {
				fmt.Printf("%s  %-5s %s", entry.Time.Format(time.RFC3339), entry.Level, entry.Message)
				for key, value := range entry.Fields {
					fmt.Printf("  %s=%s", key, value)
				}
				fmt.Println()
			}
			return nil
		},
	}

	cmd.Flags().Int64Var(&cursor, "cursor", 0, "return entries with an id greater than this cursor")
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum number of entries to return")
	cmd.Flags().BoolVar(&follow, "follow", false, "reserved for streaming; not implemented yet")

	return cmd
}
