package auth

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The lock is exercised in two ways here, because the concurrency tests
// elsewhere use goroutines in one process and are satisfied by the in-process
// mutex alone — they never touch the lock FILE, so nothing would catch its
// removal. These drive `acquireFileLock` directly against planted states, and
// spawn real processes.

// The production timings are pinned here, and shortened for the tests that must
// actually wait one out — five seconds per assertion is not worth paying on
// every run.
func TestLockTimingDefaults(t *testing.T) {
	if lockTimeout != 5*time.Second || lockStale != 30*time.Second || lockPoll != 20*time.Millisecond {
		t.Fatalf("lock timings changed: poll=%s timeout=%s stale=%s", lockPoll, lockTimeout, lockStale)
	}
}

// shortenLockTimings makes a waiting test cheap without changing its shape.
func shortenLockTimings(t *testing.T) {
	t.Helper()
	poll, timeout, stale := lockPoll, lockTimeout, lockStale
	lockPoll, lockTimeout, lockStale = 2*time.Millisecond, 150*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { lockPoll, lockTimeout, lockStale = poll, timeout, stale })
}

// childEnv names the environment variable that turns this test binary into a
// worker. Re-executing the test binary is the standard way to get real
// processes without shipping a second command.
const childEnv = "DRIFT_TEST_LOCK_CHILD"

func TestMain(m *testing.M) {
	if spec := os.Getenv(childEnv); spec != "" {
		os.Exit(runLockChild(spec))
	}
	os.Exit(m.Run())
}

// runLockChild performs one credential write and exits. `spec` is
// "<file>|<index>".
func runLockChild(spec string) int {
	parts := strings.SplitN(spec, "|", 2)
	if len(parts) != 2 {
		fmt.Fprintln(os.Stderr, "bad child spec")
		return 2
	}
	// Backend nil: the child writes to the file unconditionally, which is the
	// path the lock guards.
	s := &Store{FilePath: parts[0]}
	k := NewKey("ctx-"+parts[1], "https://drift-"+parts[1]+".example.com")
	if _, err := s.Set(k, "drift_child-token-"+parts[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// REGRESSION, and the reason the lock file needs its own test: the goroutine
// concurrency tests are satisfied by the in-process mutex, so only real
// processes prove the file lock serialises anything.
func TestFileLockSerialisesSeparateProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "credentials.yaml")

	const n = 12
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0])
			cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%s|%02d", childEnv, file, i))
			out, err := cmd.CombinedOutput()
			if err != nil {
				errs[i] = fmt.Errorf("child %d: %v: %s", i, err, out)
			}
		}(i)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	s := &Store{FilePath: file}
	for i := 0; i < n; i++ {
		k := NewKey(fmt.Sprintf("ctx-%02d", i), fmt.Sprintf("https://drift-%02d.example.com", i))
		got, err := s.Get(k)
		if err != nil {
			t.Fatalf("credential %d was lost by a concurrent process: %v", i, err)
		}
		if want := fmt.Sprintf("drift_child-token-%02d", i); got.Token != want {
			t.Fatalf("credential %d = %q, want %q", i, got.Token, want)
		}
	}
}

// HIGH regression. The staleness branch used to `continue`, skipping both the
// deadline check and the sleep. When the lock path could not be removed the
// loop spun at 100% CPU forever, and every credential write hung behind it with
// no message and no timeout to escape through.
//
// Both triggers from the report, each a realistic post-crash state.
func TestStaleLockThatCannotBeRemovedTimesOutInsteadOfSpinning(t *testing.T) {
	shortenLockTimings(t)
	t.Run("directory at the lock path", func(t *testing.T) {
		shortenLockTimings(t)
		dir := t.TempDir()
		lock := filepath.Join(dir, "credentials.yaml.lock")
		// Non-empty, so it cannot be removed.
		if err := os.MkdirAll(filepath.Join(lock, "child"), 0o700); err != nil {
			t.Fatal(err)
		}
		assertTimesOut(t, lock)
	})

	t.Run("stale lock in an unwritable directory", func(t *testing.T) {
		shortenLockTimings(t)
		if os.Geteuid() == 0 {
			t.Skip("root can unlink regardless of directory mode")
		}
		dir := t.TempDir()
		sub := filepath.Join(dir, "cfg")
		if err := os.MkdirAll(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		lock := filepath.Join(sub, "credentials.yaml.lock")
		if err := os.WriteFile(lock, []byte("1:stale"), 0o600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-2 * lockStale)
		if err := os.Chtimes(lock, old, old); err != nil {
			t.Fatal(err)
		}
		// 0500: entries can be read but not unlinked.
		if err := os.Chmod(sub, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(sub, 0o700) })
		assertTimesOut(t, lock)
	})
}

// assertTimesOut requires acquireFileLock to return an error within a bound
// comfortably above the timeout and far below "forever".
func assertTimesOut(t *testing.T, lock string) {
	t.Helper()
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := acquireFileLock(lock)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("acquired a lock that should have been unobtainable")
		}
		if elapsed := time.Since(start); elapsed > 3*lockTimeout {
			t.Fatalf("took %s to give up, timeout is %s", elapsed, lockTimeout)
		}
		if !strings.Contains(err.Error(), lock) && !strings.Contains(err.Error(), filepath.Dir(lock)) {
			t.Fatalf("the error names neither the lock nor its directory: %v", err)
		}
	case <-time.After(4 * lockTimeout):
		t.Fatalf("acquireFileLock did not return within %s; it is spinning", 4*lockTimeout)
	}
}

// LOW regression. A dangling symlink at the lock path made `Stat` fail, so the
// staleness break never fired and every write blocked the full timeout — for
// ever, since nothing ages a symlink out.
func TestDanglingSymlinkAtTheLockPathIsBroken(t *testing.T) {
	shortenLockTimings(t)
	dir := t.TempDir()
	lock := filepath.Join(dir, "credentials.yaml.lock")
	if err := os.Symlink(filepath.Join(dir, "does-not-exist"), lock); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	start := time.Now()
	held, err := acquireFileLock(lock)
	if err != nil {
		t.Fatalf("a dangling symlink blocked the lock: %v", err)
	}
	held.release()
	if elapsed := time.Since(start); elapsed > lockTimeout/2 {
		t.Fatalf("took %s, so the symlink was waited out rather than broken", elapsed)
	}
}

// A genuinely held lock is waited for and then reported, not broken.
func TestHeldLockTimesOutWithAnActionableMessage(t *testing.T) {
	shortenLockTimings(t)
	dir := t.TempDir()
	lock := filepath.Join(dir, "credentials.yaml.lock")
	held, err := acquireFileLock(lock)
	if err != nil {
		t.Fatal(err)
	}
	defer held.release()

	start := time.Now()
	if _, err := acquireFileLock(lock); err == nil {
		t.Fatal("acquired a lock someone else holds")
	} else if !strings.Contains(err.Error(), "drift process") {
		t.Fatalf("the message does not suggest a remedy: %v", err)
	}
	if elapsed := time.Since(start); elapsed < lockTimeout {
		t.Fatalf("gave up after %s, before the %s timeout", elapsed, lockTimeout)
	}
}

// A lock left by a dead process is broken and reacquired.
func TestStaleLockIsBrokenAndReacquired(t *testing.T) {
	shortenLockTimings(t)
	dir := t.TempDir()
	lock := filepath.Join(dir, "credentials.yaml.lock")
	if err := os.WriteFile(lock, []byte("999999:dead"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * lockStale)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}

	held, err := acquireFileLock(lock)
	if err != nil {
		t.Fatalf("a stale lock was not broken: %v", err)
	}
	defer held.release()
	if held.nonce == "" || !strings.HasPrefix(held.nonce, strconv.Itoa(os.Getpid())+":") {
		t.Fatalf("the lock carries no ownership token: %q", held.nonce)
	}
}

// LOW regression. `release` used to remove unconditionally, so a departing
// holder could delete a lock someone else had since acquired — leaving the
// "released" lock protecting nothing.
func TestReleaseOnlyRemovesItsOwnLock(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "credentials.yaml.lock")

	first, err := acquireFileLock(lock)
	if err != nil {
		t.Fatal(err)
	}
	// Someone else takes it over — simulated by rewriting the nonce, which is
	// what a break-and-reacquire leaves behind.
	if err := os.WriteFile(lock, []byte("4242:somebody-else"), 0o600); err != nil {
		t.Fatal(err)
	}

	first.release()
	if _, err := os.Lstat(lock); err != nil {
		t.Fatal("release removed a lock belonging to another holder")
	}
	// Its own lock it does remove.
	if err := os.WriteFile(lock, []byte(first.nonce), 0o600); err != nil {
		t.Fatal(err)
	}
	first.release()
	if _, err := os.Lstat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("release left its own lock behind")
	}
}

// MEDIUM regression. `withLock` was taken before anything else, so a read-only
// config directory failed a login the KEYRING could have served — the store
// that works made unreachable by the store that does not.
func TestReadOnlyConfigDirectoryDoesNotBlockTheKeyring(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes regardless of directory mode")
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "cfg")
	if err := os.MkdirAll(sub, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o700) })

	kr := newFakeKeyring()
	s := &Store{Backend: kr, FilePath: filepath.Join(sub, "credentials.yaml")}
	k := NewKey("alpha", "https://drift.alpha.example.com")

	src, err := s.Set(k, token)
	if err != nil {
		t.Fatalf("a read-only config directory blocked a keyring write: %v", err)
	}
	if src != SourceKeyring {
		t.Fatalf("stored in %s, want the keyring", src)
	}
	got, err := s.Get(k)
	if err != nil || got.Token != token {
		t.Fatalf("Get = %+v %v", got, err)
	}
	// Logout must work too: revocation cannot depend on a writable directory
	// when the credential is not in a file.
	if err := s.Delete(k); err != nil {
		t.Fatalf("logout blocked by the read-only directory: %v", err)
	}
}

// ...but a write that genuinely needs the file still fails, and names the
// directory rather than blaming a lock file.
func TestUnwritableDirectoryFailsTheFilePathWithAUsefulMessage(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes regardless of directory mode")
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "cfg")
	if err := os.MkdirAll(sub, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o700) })

	kr := newFakeKeyring()
	kr.unavailable = true // force the file path
	s := &Store{Backend: kr, FilePath: filepath.Join(sub, "credentials.yaml")}

	_, err := s.Set(NewKey("alpha", "https://drift.alpha.example.com"), token)
	if err == nil {
		t.Fatal("expected the file write to fail")
	}
	if !strings.Contains(err.Error(), sub) {
		t.Fatalf("the error does not name the directory: %v", err)
	}
}
