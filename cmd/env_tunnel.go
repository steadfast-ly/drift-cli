package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	chclient "github.com/jpillora/chisel/client"
	"github.com/spf13/cobra"
	"github.com/steadfast-ly/drift-cli/internal/api"
	"github.com/steadfast-ly/drift-cli/internal/client"
	"github.com/steadfast-ly/drift-cli/internal/cliexit"
)

// tunnelConfig holds the derived chisel client configuration, separated from
// the command so it can be tested as a pure function.
type tunnelConfig struct {
	ServerURL string // https://<tunnelHost>
	Remote    string // <localPort>:<dbHost>:<dbPort>
	Auth      string // chiselAuth, empty when the Install publishes none
	LocalPort int    // the port that will be bound locally
	Engine    string // "postgres" or "mysql"
	DbName    string // database name, for the client hint
}

// buildTunnelConfig maps server-supplied db-access params into a chisel config.
// portOverride, when >0, replaces the server-suggested local port.
func buildTunnelConfig(access api.DbAccessDirect, portOverride int) tunnelConfig {
	local := access.LocalPort
	if portOverride > 0 {
		local = portOverride
	}
	tc := tunnelConfig{
		ServerURL: fmt.Sprintf("https://%s", access.TunnelHost),
		Remote:    fmt.Sprintf("%d:%s:%d", local, access.DbHost, access.DbPort),
		Auth:      "", // absent by default
		LocalPort: local,
		Engine:    string(access.Engine),
		DbName:    access.DbName,
	}
	if access.ChiselAuth != nil {
		tc.Auth = *access.ChiselAuth
	}
	return tc
}

// clientHint returns an engine-aware one-liner the operator can paste.
func clientHint(tc tunnelConfig) string {
	switch tc.Engine {
	case "mysql":
		return fmt.Sprintf("mysql -h 127.0.0.1 -P %d %s --ssl-mode=REQUIRED", tc.LocalPort, tc.DbName)
	default: // postgres
		return fmt.Sprintf("psql -h 127.0.0.1 -p %d %s", tc.LocalPort, tc.DbName)
	}
}

func newEnvTunnelCommand(app *App) *cobra.Command {
	var port int

	cmd := &cobra.Command{
		Use:   "tunnel <slug-or-id>",
		Short: "Open a database tunnel to a preview environment",
		Long: "Open a chisel tunnel to the environment's database.\n\n" +
			"Fetches the connection parameters from the server, binds a local port\n" +
			"and prints a ready-to-use client command. Blocks until Ctrl-C.\n\n" +
			"Requires drift server >= 0.15.0.\n\n" + cliexit.Help,
		Args: exactArgs(1, "the environment slug or id"),
		RunE: func(c *cobra.Command, args []string) error {
			return runEnvTunnel(c.Context(), app, args[0], port)
		},
	}
	cmd.Flags().IntVar(&port, "port", 0, "local port to bind (default: server-suggested)")
	return cmd
}

func runEnvTunnel(ctx context.Context, app *App, ref string, portOverride int) error {
	if portOverride != 0 && (portOverride < 1 || portOverride > 65535) {
		return usageErrorf("--port must be between 1 and 65535 (got %d)", portOverride)
	}

	sess, err := app.Connect(ctx, FeatureEnvironmentsRead)
	if err != nil {
		return err
	}

	resp, err := sess.API.EnvironmentsDbAccessWithResponse(ctx, ref)
	if err != nil {
		return client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		e := client.Fail(resp, resp.Headers429)
		if resp.JSON404 != nil {
			// A decodable 404: the server knows the endpoint but the ref is
			// not found. Hint at slug-resolution semantics.
			e.Hint = fmt.Sprintf(
				"a slug resolves only live environments; if %q was destroyed or canceled, address it by id", ref)
		} else if resp.StatusCode() == 404 {
			// An undecodable 404: the server does not have this endpoint at
			// all (old server returning HTML). Override the generic hint with
			// a version floor.
			e.Hint = "this command requires drift server >= 0.15.0; run `drift doctor` to check the server version"
		}
		return e
	}

	// The response is an anyOf union. Try the published-true variant first.
	published, err := resp.JSON200.AsEnvironmentsDbAccess200JSONResponseBody0()
	if err != nil {
		// Try the published-false variant.
		unpub, err2 := resp.JSON200.AsEnvironmentsDbAccess200JSONResponseBody1()
		if err2 != nil {
			return &cliexit.ExitError{
				Code:    cliexit.Error,
				Message: "unexpected response shape from the db-access endpoint",
				Hint:    "this command requires drift server >= 0.15.0; run `drift doctor` to check",
			}
		}
		_ = unpub
		return &cliexit.ExitError{
			Code:    cliexit.Error,
			Message: "this install publishes no database access for preview environments",
		}
	}

	// Check the published field: a zero-value interface{} is nil when
	// published is false (the anyOf discriminator). If we successfully decoded
	// variant 0 but the access struct is empty (tunnelHost == ""), try
	// variant 1.
	if published.Access.TunnelHost == "" {
		return &cliexit.ExitError{
			Code:    cliexit.Error,
			Message: "this install publishes no database access for preview environments",
		}
	}

	tc := buildTunnelConfig(published.Access, portOverride)

	// Set up signal handling scoped to this command. The brief says to use
	// signal.NotifyContext on the command's context rather than rewriting
	// main/Execute.
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Build and start the chisel client.
	cfg := &chclient.Config{
		Server:        tc.ServerURL,
		Remotes:       []string{tc.Remote},
		Auth:          tc.Auth,
		KeepAlive:     25 * time.Second,
		MaxRetryCount: 0, // unlimited retries
	}
	ch, err := chclient.NewClient(cfg)
	if err != nil {
		return &cliexit.ExitError{
			Code:    cliexit.Error,
			Message: fmt.Sprintf("cannot configure tunnel to %s: %s", tc.ServerURL, err),
		}
	}

	if err := ch.Start(sigCtx); err != nil {
		return &cliexit.ExitError{
			Code:    cliexit.Error,
			Message: fmt.Sprintf("cannot start tunnel to %s: %s", tc.ServerURL, err),
		}
	}

	// Wait for the tunnel to be ready (or context cancellation).
	if !ch.Ready(sigCtx) {
		// Context was cancelled before the tunnel connected.
		_ = ch.Close()
		return nil
	}

	// Print the bound port and client hint.
	fmt.Fprintf(app.Stdout, "Tunnel ready on 127.0.0.1:%d\n", tc.LocalPort)
	fmt.Fprintf(app.Stdout, "  %s\n", clientHint(tc))

	// Block until Ctrl-C.
	<-sigCtx.Done()

	// Clean close.
	_ = ch.Close()
	return nil
}
