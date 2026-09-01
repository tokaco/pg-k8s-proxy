// Command pg-k8s-proxy is a Kubernetes operator and gateway that routes
// PostgreSQL connections to the right instance based on the database name in
// the client's startup message.
//
// One binary carries two roles. The manager role reconciles PostgresRoute
// objects and adopts labelled Services; the proxy role serves client traffic.
// By default a process runs both, which is what the Helm chart deploys.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/tokaco/pg-k8s-proxy/internal/config"
	"github.com/tokaco/pg-k8s-proxy/internal/version"
)

func main() {
	if err := newRootCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	cfg := config.Default()

	cmd := &cobra.Command{
		Use:   "pg-k8s-proxy",
		Short: "PostgreSQL gateway and Kubernetes operator",
		Long: "pg-k8s-proxy routes PostgreSQL client connections to the instance that serves\n" +
			"the requested database, using PostgresRoute objects and, optionally, labelled\n" +
			"Services as its routing table.",
		Version:       version.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			warnings := cfg.ApplyEnvironment(func(name string) bool {
				return cmd.Flags().Changed(name)
			})
			if err := cfg.Validate(); err != nil {
				return err
			}
			return run(cmd.Context(), cfg, warnings)
		},
	}

	cfg.BindFlags(cmd.Flags())
	cmd.SetVersionTemplate(version.String() + "\n")
	cmd.AddCommand(newVersionCommand())
	return cmd
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), version.String())
			return err
		},
	}
}
