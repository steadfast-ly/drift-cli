package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/steadfast-ly/drift-cli/internal/cliexit"
)

func newEnvTunnelCommand(app *App) *cobra.Command {
	var port int

	cmd := &cobra.Command{
		Use:   "tunnel <slug-or-id>",
		Short: "Open a database tunnel to a preview environment",
		Long: "Open a chisel tunnel to the environment's database.\n\n" +
			"Fetches the connection parameters from the server, binds a local port\n" +
			"and prints a ready-to-use client command. Blocks until Ctrl-C.\n\n" +
			"Requires drift server >= " + dbAccessVersionHint + ".\n\n" + cliexit.Help,
		Args: exactArgs(1, "the environment slug or id"),
		RunE: func(c *cobra.Command, args []string) error {
			return runEnvTunnel(c.Context(), app, args[0], port)
		},
	}
	cmd.Flags().IntVar(&port, "port", 0, "local port to bind (default: server-suggested)")
	return cmd
}

func runEnvTunnel(ctx context.Context, app *App, ref string, portOverride int) error {
	if err := validatePort(portOverride); err != nil {
		return err
	}

	sess, err := app.Connect(ctx, FeatureEnvironmentsRead)
	if err != nil {
		return err
	}

	tc, err := fetchDbAccess(ctx, sess, ref, portOverride)
	if err != nil {
		return err
	}

	// Set up signal handling scoped to this command.
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	ch, err := startTunnel(sigCtx, tc)
	if err != nil {
		return err
	}

	// Wait for the tunnel to be ready (or context cancellation).
	if !ch.Ready(sigCtx) {
		_ = ch.Close()
		return nil
	}

	// Print the bound port and client hint.
	fmt.Fprintf(app.Stdout, "Tunnel ready on 127.0.0.1:%d\n", tc.LocalPort)
	fmt.Fprintf(app.Stdout, "  %s\n", clientHint(*tc))

	// Block until Ctrl-C.
	<-sigCtx.Done()

	// Clean close.
	_ = ch.Close()
	return nil
}
