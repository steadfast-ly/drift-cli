package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steadfast-ly/drift-cli/internal/cliexit"
)

// --- pure argv-builder tests ------------------------------------------------

func TestBuildPsqlArgs(t *testing.T) {
	t.Run("no user", func(t *testing.T) {
		args := buildPsqlArgs(33060, "preview_db", "")
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "-h 127.0.0.1") {
			t.Fatalf("missing -h: %s", joined)
		}
		if !strings.Contains(joined, "-p 33060") {
			t.Fatalf("missing -p: %s", joined)
		}
		if !strings.Contains(joined, "-d preview_db") {
			t.Fatalf("missing -d: %s", joined)
		}
		if strings.Contains(joined, "-U") {
			t.Fatalf("must NOT emit -U when user is empty: %s", joined)
		}
	})

	t.Run("with user", func(t *testing.T) {
		args := buildPsqlArgs(33060, "preview_db", "dbadmin")
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "-U dbadmin") {
			t.Fatalf("missing -U dbadmin: %s", joined)
		}
	})
}

func TestBuildMysqlArgs(t *testing.T) {
	t.Run("oracle no user", func(t *testing.T) {
		args := buildMysqlArgs(33060, "preview_db", "", mysqlOracle)
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "--ssl-mode=REQUIRED") {
			t.Fatalf("Oracle mysql must use --ssl-mode=REQUIRED: %s", joined)
		}
		if strings.Contains(joined, "ssl-verify-server-cert") {
			t.Fatalf("Oracle mysql must NOT use --ssl-verify-server-cert: %s", joined)
		}
		if !strings.Contains(joined, "--protocol=TCP") {
			t.Fatalf("missing --protocol=TCP: %s", joined)
		}
		if !strings.Contains(joined, "-P 33060") {
			t.Fatalf("missing -P: %s", joined)
		}
		if !strings.Contains(joined, "-D preview_db") {
			t.Fatalf("missing -D: %s", joined)
		}
		if !strings.Contains(joined, "-p") {
			t.Fatalf("missing -p (password prompt): %s", joined)
		}
		if strings.Contains(joined, "-u") {
			t.Fatalf("must NOT emit -u when user is empty: %s", joined)
		}
	})

	t.Run("oracle with user", func(t *testing.T) {
		args := buildMysqlArgs(33060, "preview_db", "dbadmin", mysqlOracle)
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "-u dbadmin") {
			t.Fatalf("missing -u dbadmin: %s", joined)
		}
	})

	t.Run("mariadb no user", func(t *testing.T) {
		args := buildMysqlArgs(33060, "preview_db", "", mysqlMariaDB)
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "--ssl-verify-server-cert=0") {
			t.Fatalf("MariaDB must use --ssl-verify-server-cert=0: %s", joined)
		}
		if !strings.Contains(joined, "--ssl") {
			t.Fatalf("MariaDB must use --ssl: %s", joined)
		}
		if strings.Contains(joined, "--ssl-mode") {
			t.Fatalf("MariaDB must NOT use --ssl-mode: %s", joined)
		}
		if !strings.Contains(joined, "--protocol=TCP") {
			t.Fatalf("missing --protocol=TCP: %s", joined)
		}
		if strings.Contains(joined, "-u") {
			t.Fatalf("must NOT emit -u when user is empty: %s", joined)
		}
	})

	t.Run("mariadb with user", func(t *testing.T) {
		args := buildMysqlArgs(33060, "preview_db", "root", mysqlMariaDB)
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "-u root") {
			t.Fatalf("missing -u root: %s", joined)
		}
	})
}

func TestResolveDbClient(t *testing.T) {
	t.Run("postgres found", func(t *testing.T) {
		lookup := func(name string) (string, error) {
			if name == "psql" {
				return "/usr/bin/psql", nil
			}
			return "", fmt.Errorf("not found: %s", name)
		}
		argv, err := resolveDbClient("postgres", 33060, "mydb", "", lookup, nil)
		if err != nil {
			t.Fatal(err)
		}
		if argv.Binary != "/usr/bin/psql" {
			t.Fatalf("Binary = %q", argv.Binary)
		}
		if !strings.Contains(strings.Join(argv.Args, " "), "-p 33060") {
			t.Fatalf("Args = %v", argv.Args)
		}
	})

	t.Run("postgres missing", func(t *testing.T) {
		lookup := func(string) (string, error) {
			return "", fmt.Errorf("not found")
		}
		_, err := resolveDbClient("postgres", 33060, "mydb", "", lookup, nil)
		if err == nil {
			t.Fatal("expected error")
		}
		if cliexit.CodeOf(err) != cliexit.Usage {
			t.Fatalf("exit %d, want %d", cliexit.CodeOf(err), cliexit.Usage)
		}
		if !strings.Contains(err.Error(), "psql") {
			t.Fatalf("error does not name the binary: %s", err)
		}
	})

	t.Run("mysql found oracle", func(t *testing.T) {
		lookup := func(name string) (string, error) {
			if name == "mysql" {
				return "/usr/bin/mysql", nil
			}
			return "", fmt.Errorf("not found")
		}
		detect := func(string) mysqlBinaryKind { return mysqlOracle }
		argv, err := resolveDbClient("mysql", 33060, "mydb", "", lookup, detect)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(argv.Args, " ")
		if !strings.Contains(joined, "--ssl-mode=REQUIRED") {
			t.Fatalf("Oracle flags expected: %s", joined)
		}
	})

	t.Run("mysql found mariadb via detect", func(t *testing.T) {
		lookup := func(name string) (string, error) {
			if name == "mysql" {
				return "/usr/bin/mysql", nil
			}
			return "", fmt.Errorf("not found")
		}
		detect := func(string) mysqlBinaryKind { return mysqlMariaDB }
		argv, err := resolveDbClient("mysql", 33060, "mydb", "", lookup, detect)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(argv.Args, " ")
		if !strings.Contains(joined, "--ssl-verify-server-cert=0") {
			t.Fatalf("MariaDB flags expected: %s", joined)
		}
	})

	t.Run("mysql fallback to mariadb binary", func(t *testing.T) {
		lookup := func(name string) (string, error) {
			if name == "mariadb" {
				return "/usr/bin/mariadb", nil
			}
			return "", fmt.Errorf("not found")
		}
		// detect should NOT be called when mariadb binary is used directly
		detect := func(string) mysqlBinaryKind {
			t.Fatal("detect should not be called for the mariadb binary")
			return mysqlOracle
		}
		argv, err := resolveDbClient("mysql", 33060, "mydb", "", lookup, detect)
		if err != nil {
			t.Fatal(err)
		}
		if argv.Binary != "/usr/bin/mariadb" {
			t.Fatalf("Binary = %q, want /usr/bin/mariadb", argv.Binary)
		}
		joined := strings.Join(argv.Args, " ")
		if !strings.Contains(joined, "--ssl-verify-server-cert=0") {
			t.Fatalf("MariaDB flags expected: %s", joined)
		}
	})

	t.Run("mysql neither found", func(t *testing.T) {
		lookup := func(string) (string, error) {
			return "", fmt.Errorf("not found")
		}
		_, err := resolveDbClient("mysql", 33060, "mydb", "", lookup, nil)
		if err == nil {
			t.Fatal("expected error")
		}
		if cliexit.CodeOf(err) != cliexit.Usage {
			t.Fatalf("exit %d, want %d", cliexit.CodeOf(err), cliexit.Usage)
		}
		errMsg := err.Error()
		if !strings.Contains(errMsg, "mysql") || !strings.Contains(errMsg, "mariadb") {
			t.Fatalf("error does not name both binaries: %s", errMsg)
		}
	})
}

// --- end-to-end through the fake server -------------------------------------

// TestDbPublishedFalse proves env db shares the same published:false
// handling as env tunnel (via the extracted fetchDbAccess helper).
func TestDbPublishedFalse(t *testing.T) {
	srv := newDbAccessFake(t, "unpublished")
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	_, errOut, code := h.run("env", "db", "proof-alpha")
	if code != cliexit.Error {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.Error, errOut)
	}
	if !strings.Contains(errOut, "no database access") {
		t.Fatalf("error does not explain the condition: %s", errOut)
	}
}

// TestDbNotFound proves env db shares 404 → exit 3 with env tunnel.
func TestDbNotFound(t *testing.T) {
	srv := newDbAccessFake(t, "404")
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	_, errOut, code := h.run("env", "db", "nope")
	if code != cliexit.NotFound {
		t.Fatalf("exit %d, want %d\n%s", code, cliexit.NotFound, errOut)
	}
}

// TestDbMissingArgument verifies that no slug → exit 2.
func TestDbMissingArgument(t *testing.T) {
	srv := newDbAccessFake(t, "published")
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	_, _, code := h.run("env", "db")
	if code != cliexit.Usage {
		t.Fatalf("exit %d, want %d", code, cliexit.Usage)
	}
}

// TestDbPortValidation mirrors the tunnel's port validation test.
func TestDbPortValidation(t *testing.T) {
	srv := newDbAccessFake(t, "published")
	h := newHarness(t)
	h.setup(t, srv, goodToken)

	for _, port := range []string{"-1", "70000"} {
		t.Run(port, func(t *testing.T) {
			_, errOut, code := h.run("env", "db", "proof-alpha", "--port", port)
			if code != cliexit.Usage {
				t.Fatalf("exit %d, want %d\n%s", code, cliexit.Usage, errOut)
			}
			if !strings.Contains(errOut, "--port") {
				t.Fatalf("error does not name the flag: %s", errOut)
			}
		})
	}
}

// --- client exit-code propagation -------------------------------------------

// TestDbClientExitCodePropagation tests that mapClientExit propagates a real
// exec.ExitError's code. We run a tiny script that exits with a known code
// and verify mapClientExit returns the same code.
func TestDbClientExitCodePropagation(t *testing.T) {
	binDir := t.TempDir()

	// A script that exits 42.
	script := filepath.Join(binDir, "fail42")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 42\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Run the script and capture the exec error.
	cmd := exec.Command(script)
	runErr := cmd.Run()
	if runErr == nil {
		t.Fatal("expected exit 42, got success")
	}

	// mapClientExit must propagate code 42.
	mapped := mapClientExit(script, runErr)
	if cliexit.CodeOf(mapped) != 42 {
		t.Fatalf("mapClientExit code = %d, want 42", cliexit.CodeOf(mapped))
	}

	// Success path.
	if mapClientExit(script, nil) != nil {
		t.Fatal("mapClientExit(nil) must be nil")
	}
}

// TestDbClientSignalKilled verifies that a signal-terminated client yields
// exit 128+N (shell convention).
func TestDbClientSignalKilled(t *testing.T) {
	binDir := t.TempDir()

	// A script that kills itself with SIGTERM (signal 15 → exit 143).
	script := filepath.Join(binDir, "killself")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nkill -TERM $$\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(script)
	runErr := cmd.Run()
	if runErr == nil {
		t.Fatal("expected signal death, got success")
	}

	mapped := mapClientExit(script, runErr)
	code := cliexit.CodeOf(mapped)
	// SIGTERM = 15, so 128+15 = 143.
	if code != 143 {
		t.Fatalf("mapClientExit code = %d, want 143 (128+SIGTERM)", code)
	}
}

// --- helpers shared with tunnel tests (re-verify sharing) -------------------
// The tests above use the same newDbAccessFake from env_tunnel_test.go,
// proving the shared fetchDbAccess helper produces identical behavior.
