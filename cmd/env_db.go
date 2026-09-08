package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/steadfast-ly/drift-cli/internal/cliexit"
)

func newEnvDbCommand(app *App) *cobra.Command {
	var port int
	var user string

	cmd := &cobra.Command{
		Use:   "db <slug-or-id>",
		Short: "Open an interactive database session on a preview environment",
		Long: "Open a chisel tunnel and launch the database client.\n\n" +
			"Everything `drift env tunnel` does, then exec's the interactive\n" +
			"client (psql for Postgres, mysql/mariadb for MySQL). The tunnel\n" +
			"is closed when the client exits, and the client's exit code is\n" +
			"propagated.\n\n" +
			"--user sets the database username. When omitted, the client\n" +
			"uses its own default (typically the OS login name).\n\n" +
			"The password is left to the client's own prompt — drift resolves\n" +
			"no credentials beyond the tunnel.\n\n" +
			"Requires drift server >= " + dbAccessVersionHint + ".\n\n" + cliexit.Help,
		Args: exactArgs(1, "the environment slug or id"),
		RunE: func(c *cobra.Command, args []string) error {
			return runEnvDb(c.Context(), app, args[0], port, user)
		},
	}
	cmd.Flags().IntVar(&port, "port", 0, "local port to bind (default: server-suggested)")
	cmd.Flags().StringVar(&user, "user", "", "database username; defaults to the client's own default, typically your OS login name")
	return cmd
}

// dbClientArgv is the fully resolved argv for a database client. Separated as
// a pure function so it is testable without a live tunnel or a real binary.
type dbClientArgv struct {
	Binary string   // resolved path to the binary
	Args   []string // arguments (not including the binary itself)
}

// mysqlBinaryKind distinguishes the Oracle and MariaDB mysql clients, which
// need different TLS flags.
type mysqlBinaryKind int

const (
	mysqlOracle  mysqlBinaryKind = iota // Oracle mysql client
	mysqlMariaDB                        // MariaDB mysql/mariadb client
)

// detectMysqlKind determines whether a mysql binary is Oracle or MariaDB by
// inspecting its --version output. MariaDB's output contains "MariaDB" or
// "mariadb"; Oracle's does not.
func detectMysqlKind(binary string) mysqlBinaryKind {
	out, err := exec.Command(binary, "--version").Output()
	if err != nil {
		// Cannot determine; default to Oracle flags which are the safer
		// choice (--ssl-mode=REQUIRED is widely understood).
		return mysqlOracle
	}
	if strings.Contains(strings.ToLower(string(out)), "mariadb") {
		return mysqlMariaDB
	}
	return mysqlOracle
}

// buildMysqlArgs returns the argv (without binary) for the mysql/mariadb
// client. TLS per drift#32: keep TLS on, skip server-cert verification.
//
//   - Oracle mysql: --ssl-mode=REQUIRED (encrypts, no cert verify)
//   - MariaDB: --ssl --ssl-verify-server-cert=0 (encrypts, no cert verify)
//
// The two flag sets are NOT interchangeable.
func buildMysqlArgs(localPort int, dbName, user string, kind mysqlBinaryKind) []string {
	args := []string{
		"-h", "127.0.0.1",
		"-P", fmt.Sprintf("%d", localPort),
		"--protocol=TCP",
		"-p",
		"-D", dbName,
	}
	if user != "" {
		args = append(args, "-u", user)
	}
	switch kind {
	case mysqlMariaDB:
		args = append(args, "--ssl", "--ssl-verify-server-cert=0")
	default: // Oracle
		args = append(args, "--ssl-mode=REQUIRED")
	}
	return args
}

// buildPsqlArgs returns the argv (without binary) for the psql client.
// No TLS flags needed — the local tunnel is plaintext to localhost, and the
// chisel tunnel handles the encryption to the server.
func buildPsqlArgs(localPort int, dbName, user string) []string {
	args := []string{
		"-h", "127.0.0.1",
		"-p", fmt.Sprintf("%d", localPort),
		"-d", dbName,
	}
	if user != "" {
		args = append(args, "-U", user)
	}
	return args
}

// lookupFunc is the signature of exec.LookPath, injectable for tests.
type lookupFunc func(string) (string, error)

// detectFunc is the signature of detectMysqlKind, injectable for tests.
type detectFunc func(string) mysqlBinaryKind

// resolveDbClient picks the binary and builds the argv for the engine.
// lookup and detect are injectable for testing.
func resolveDbClient(engine string, localPort int, dbName, user string, lookup lookupFunc, detect detectFunc) (*dbClientArgv, error) {
	switch engine {
	case "postgres":
		bin, err := lookup("psql")
		if err != nil {
			return nil, &cliexit.ExitError{
				Code:    cliexit.Usage,
				Message: "psql is not installed or not on PATH",
				Hint:    "install PostgreSQL client tools (e.g. `apt install postgresql-client` or `brew install libpq`)",
			}
		}
		return &dbClientArgv{
			Binary: bin,
			Args:   buildPsqlArgs(localPort, dbName, user),
		}, nil

	case "mysql":
		// Prefer `mysql`, fall back to `mariadb` (D6).
		bin, err := lookup("mysql")
		if err != nil {
			bin, err = lookup("mariadb")
			if err != nil {
				return nil, &cliexit.ExitError{
					Code:    cliexit.Usage,
					Message: "neither mysql nor mariadb is installed or on PATH",
					Hint:    "install a MySQL client (e.g. `apt install mysql-client` or `brew install mysql-client`)",
				}
			}
			// The `mariadb` binary is always MariaDB.
			return &dbClientArgv{
				Binary: bin,
				Args:   buildMysqlArgs(localPort, dbName, user, mysqlMariaDB),
			}, nil
		}
		// `mysql` could be either Oracle or MariaDB (some distros ship
		// MariaDB as `mysql`). Detect and choose flags accordingly.
		kind := detect(bin)
		return &dbClientArgv{
			Binary: bin,
			Args:   buildMysqlArgs(localPort, dbName, user, kind),
		}, nil

	default:
		return nil, &cliexit.ExitError{
			Code:    cliexit.Error,
			Message: fmt.Sprintf("unsupported database engine %q", engine),
		}
	}
}

func runEnvDb(ctx context.Context, app *App, ref string, portOverride int, user string) error {
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

	// Resolve the client binary BEFORE opening the tunnel, so a missing
	// binary fails fast without leaving an orphaned tunnel.
	argv, err := resolveDbClient(tc.Engine, tc.LocalPort, tc.DbName, user, exec.LookPath, detectMysqlKind)
	if err != nil {
		return err
	}

	// Signal handling: sigCtx drives the tunnel lifetime. The deferred
	// stop() fires after the client exits, which cancels sigCtx and tears
	// the tunnel down at the right time.
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	ch, err := startTunnel(sigCtx, tc)
	if err != nil {
		return err
	}
	defer func() { _ = ch.Close() }()

	// Wait for the tunnel to be ready.
	if !ch.Ready(sigCtx) {
		return nil
	}

	// Announce what we're about to launch (stderr, so it doesn't pollute
	// a piped session).
	fmt.Fprintf(app.Stderr, "Tunnel ready on 127.0.0.1:%d, launching %s\n", tc.LocalPort, argv.Binary)

	// Swallow SIGINT/SIGTERM in the parent while the child runs.
	//
	// signal.Ignore is wrong here: it sets SIG_IGN, which is INHERITED
	// across exec, so the child (psql/mysql) would start with SIGINT
	// ignored and Ctrl-C query-cancel would be dead.
	//
	// Instead, keep a signal.Notify registration that intercepts the
	// signals — the parent survives, the child (whose disposition exec
	// resets to SIG_DFL) handles its own Ctrl-C. A draining goroutine
	// discards SIGINT and forwards SIGTERM to the child process.
	//
	// signal.Reset undoes our NotifyContext registration for these signals
	// WITHOUT cancelling sigCtx (unlike stop(), which would tear down the
	// tunnel).
	signal.Reset(os.Interrupt, syscall.SIGTERM)
	swallow := make(chan os.Signal, 1)
	signal.Notify(swallow, os.Interrupt, syscall.SIGTERM)

	// Exec the client with stdin/stdout/stderr attached.
	cmd := exec.CommandContext(ctx, argv.Binary, argv.Args...)
	cmd.Stdin = app.Stdin
	cmd.Stdout = app.Stdout
	cmd.Stderr = app.Stderr

	// Drain signals: discard SIGINT; forward SIGTERM to the child.
	go func() {
		for sig := range swallow {
			if sig == syscall.SIGTERM && cmd.Process != nil {
				_ = cmd.Process.Signal(syscall.SIGTERM)
			}
			// SIGINT is swallowed — the child got it from the terminal.
		}
	}()

	err = cmd.Run()

	signal.Stop(swallow)
	close(swallow)

	return mapClientExit(argv.Binary, err)
}

// mapClientExit translates a client process result into the CLI's exit-code
// contract. A nil error is success; an ExitError propagates the client's own
// code (or 128+signal when terminated by a signal); anything else is a
// generic error.
func mapClientExit(binary string, err error) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code := exitErr.ExitCode()
		if code >= 0 {
			return &cliexit.ExitError{
				Code:    code,
				Message: fmt.Sprintf("%s exited %d", binary, code),
			}
		}
		// ExitCode() returns -1 when the process was killed by a signal.
		// Recover the signal number from the WaitStatus when available
		// (Unix), and follow the shell convention of 128+N.
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			code = 128 + int(ws.Signal())
			return &cliexit.ExitError{
				Code:    code,
				Message: fmt.Sprintf("%s killed by signal %d", binary, ws.Signal()),
			}
		}
		return &cliexit.ExitError{
			Code:    cliexit.Error,
			Message: fmt.Sprintf("%s terminated abnormally", binary),
		}
	}
	return &cliexit.ExitError{
		Code:    cliexit.Error,
		Message: fmt.Sprintf("failed to run %s: %s", binary, err),
	}
}
