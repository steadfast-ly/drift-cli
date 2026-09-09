package cmd

import (
	"context"
	"fmt"
	"time"

	chclient "github.com/jpillora/chisel/client"
	"github.com/steadfast-ly/drift-cli/internal/api"
	"github.com/steadfast-ly/drift-cli/internal/client"
	"github.com/steadfast-ly/drift-cli/internal/cliexit"
	"github.com/steadfast-ly/drift-cli/internal/discovery"
)

// tunnelConfig holds the derived chisel client configuration, separated from
// the command so it can be tested as a pure function.
type tunnelConfig struct {
	ServerURL string // https://<tunnelHost>
	Remote    string // 127.0.0.1:<localPort>:<dbHost>:<dbPort>
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
		Remote:    fmt.Sprintf("127.0.0.1:%d:%s:%d", local, access.DbHost, access.DbPort),
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
		return fmt.Sprintf("mysql -h 127.0.0.1 -P %d %s --ssl-mode=REQUIRED   (MariaDB: --ssl --ssl-verify-server-cert=0)", tc.LocalPort, tc.DbName)
	default: // postgres
		return fmt.Sprintf("psql -h 127.0.0.1 -p %d %s", tc.LocalPort, tc.DbName)
	}
}

// dbAccessVersionHint is the minimum server version that carries the
// db-access endpoint. Named once so the tunnel and db commands agree.
const dbAccessVersionHint = "0.15.0"

// fetchDbAccess fetches the db-access params and returns the tunnelConfig.
// Error handling (published:false, 404, old server) is identical across env
// tunnel and env db — factored here so they cannot diverge.
func fetchDbAccess(ctx context.Context, sess *Session, ref string, portOverride int) (*tunnelConfig, error) {
	resp, err := sess.API.EnvironmentsDbAccessWithResponse(ctx, ref)
	if err != nil {
		return nil, client.Transport(err, sess.Resolved.Endpoint)
	}
	if resp.JSON200 == nil {
		e := client.Fail(resp, resp.Headers429)
		if resp.StatusCode() == 404 {
			// Disambiguate route-miss from env-not-found.
			//
			// A pre-0.15.0 server has no /db-access route, so its
			// catch-all returns a decodable 404 problem envelope with the
			// same URN as a real env-not-found. Branching on the message
			// text would be fragile, so we use the discovered server
			// version: when the server is known to be below the version
			// that introduced the endpoint, ANY 404 is a route miss.
			serverVersion := ""
			if sess.Discovery != nil && sess.Discovery.Document != nil {
				serverVersion = sess.Discovery.Document.Version
			}
			if serverVersion != "" && discovery.VersionBefore(serverVersion, dbAccessVersionHint) {
				e.Hint = fmt.Sprintf(
					"this command requires drift server >= %s; this server reports %s — run `drift doctor`",
					dbAccessVersionHint, serverVersion)
			} else if resp.JSON404 != nil {
				// Server is >= 0.15.0 (or unknown) and sent a typed 404:
				// the route exists but the environment was not found.
				e.Hint = fmt.Sprintf(
					"a slug resolves only live environments; if %q was destroyed or canceled, address it by id", ref)
			} else {
				// Undecodable 404 from a server whose version we don't
				// know — fall back to the version hint.
				e.Hint = fmt.Sprintf(
					"this command requires drift server >= %s; run `drift doctor` to check the server version",
					dbAccessVersionHint)
			}
		}
		return nil, e
	}

	// The response is an anyOf union. Try the published-true variant first.
	published, err := resp.JSON200.AsEnvironmentsDbAccess200JSONResponseBody0()
	if err != nil {
		// Try the published-false variant.
		unpub, err2 := resp.JSON200.AsEnvironmentsDbAccess200JSONResponseBody1()
		if err2 != nil {
			return nil, &cliexit.ExitError{
				Code:    cliexit.Error,
				Message: "unexpected response shape from the db-access endpoint",
				Hint:    fmt.Sprintf("this command requires drift server >= %s; run `drift doctor` to check", dbAccessVersionHint),
			}
		}
		_ = unpub
		return nil, &cliexit.ExitError{
			Code:    cliexit.Error,
			Message: "this install publishes no database access for preview environments",
		}
	}

	// If we successfully decoded variant 0 but the access struct is empty
	// (tunnelHost == ""), the server returned published:false in disguise.
	if published.Access.TunnelHost == "" {
		return nil, &cliexit.ExitError{
			Code:    cliexit.Error,
			Message: "this install publishes no database access for preview environments",
		}
	}

	tc := buildTunnelConfig(published.Access, portOverride)
	return &tc, nil
}

// startTunnel builds and starts a chisel client. The caller owns the context
// and closing the client.
func startTunnel(ctx context.Context, tc *tunnelConfig) (*chclient.Client, error) {
	cfg := &chclient.Config{
		Server:        tc.ServerURL,
		Remotes:       []string{tc.Remote},
		Auth:          tc.Auth,
		KeepAlive:     25 * time.Second,
		MaxRetryCount: 0, // unlimited retries
	}
	ch, err := chclient.NewClient(cfg)
	if err != nil {
		return nil, &cliexit.ExitError{
			Code:    cliexit.Error,
			Message: fmt.Sprintf("cannot configure tunnel to %s: %s", tc.ServerURL, err),
		}
	}

	if err := ch.Start(ctx); err != nil {
		return nil, &cliexit.ExitError{
			Code:    cliexit.Error,
			Message: fmt.Sprintf("cannot start tunnel to %s: %s", tc.ServerURL, err),
		}
	}

	return ch, nil
}

// validatePort checks that a non-zero port override is in the valid range.
func validatePort(port int) error {
	if port != 0 && (port < 1 || port > 65535) {
		return usageErrorf("--port must be between 1 and 65535 (got %d)", port)
	}
	return nil
}
