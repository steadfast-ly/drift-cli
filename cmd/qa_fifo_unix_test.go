//go:build unix

package cmd

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/steadfast-ly/drift-cli/internal/cliexit"
)

// Opening a FIFO with no writer blocks forever, so a --file naming one must be
// refused before it is opened.
func TestQaAttachmentsRejectFifoWithoutBlocking(t *testing.T) {
	srv := newQaServer(t)
	h := newQaHarness(t, srv)
	fifo := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}

	type result struct {
		errOut string
		code   int
	}
	done := make(chan result, 1)
	go func() {
		_, errOut, code := h.run("qa", "approve", qaSessionID,
			"--revision", "1", "--retested", "--file", fifo, "--yes")
		done <- result{errOut, code}
	}()

	select {
	case r := <-done:
		if r.code != cliexit.Usage {
			t.Fatalf("exit %d, want %d\n%s", r.code, cliexit.Usage, r.errOut)
		}
		if !strings.Contains(r.errOut, "is not a regular file") {
			t.Fatalf("unhelpful error:\n%s", r.errOut)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("qa approve blocked opening a FIFO")
	}
	if n := srv.seen("approve"); n != 0 {
		t.Fatalf("%d approve request(s) sent", n)
	}
}
