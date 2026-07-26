package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeKeyring is an in-memory Backend. Tests must never touch the developer's
// real keyring, and a CI runner has none at all, so the OS backend is replaced
// rather than mocked at the library's global level.
type fakeKeyring struct {
	items map[string]string
	// failSet and failDelete inject a store-level failure that is NOT absence.
	failSet    error
	failDelete error
	// unavailable simulates a machine with no Secret Service: every operation
	// fails, which is the headless-Linux case the file fallback exists for.
	unavailable bool
	sets        int
	deletes     int
}

var errNoKeyring = errors.New("no keyring available")

// The fake honours the Backend contract: absence is ErrNoCredential, and a
// store that cannot be reached is anything else. Delete depends on the
// difference, so a fake that blurred it would hide the bug it must catch.

func newFakeKeyring() *fakeKeyring { return &fakeKeyring{items: map[string]string{}} }

func (f *fakeKeyring) key(service, user string) string { return service + "\x00" + user }

func (f *fakeKeyring) Get(service, user string) (string, error) {
	if f.unavailable {
		return "", errNoKeyring
	}
	v, ok := f.items[f.key(service, user)]
	if !ok {
		return "", fmt.Errorf("%w", ErrNoCredential)
	}
	return v, nil
}

func (f *fakeKeyring) Set(service, user, password string) error {
	if f.unavailable {
		return errNoKeyring
	}
	if f.failSet != nil {
		return f.failSet
	}
	f.sets++
	f.items[f.key(service, user)] = password
	return nil
}

func (f *fakeKeyring) Delete(service, user string) error {
	if f.unavailable {
		return errNoKeyring
	}
	if f.failDelete != nil {
		return f.failDelete
	}
	if _, ok := f.items[f.key(service, user)]; !ok {
		return fmt.Errorf("%w", ErrNoCredential)
	}
	f.deletes++
	delete(f.items, f.key(service, user))
	return nil
}

const token = "drift_UDkxSFRqTFJvUnpEcVpqOGdWcW1BUT09"

// Credentials are keyed on the context name AND the endpoint, so the tests need
// real keys rather than bare names.
var (
	auKey = NewKey("au", "https://drift.au.example.com")
	enKey = NewKey("en", "https://drift.en.example.com")
)

func newStore(t *testing.T, kr Backend) *Store {
	t.Helper()
	return &Store{Backend: kr, FilePath: filepath.Join(t.TempDir(), "credentials.yaml")}
}

func TestKeyringRoundTrip(t *testing.T) {
	kr := newFakeKeyring()
	s := newStore(t, kr)

	src, err := s.Set(auKey, token)
	if err != nil {
		t.Fatal(err)
	}
	if src != SourceKeyring {
		t.Fatalf("stored in %s, want the keyring", src)
	}
	// Nothing may be written to disk when the keyring accepted the credential.
	if _, err := os.Stat(s.FilePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a credential file was created alongside the keyring entry")
	}

	got, err := s.Get(auKey)
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != token || got.Source != SourceKeyring {
		t.Fatalf("Get = %+v", got)
	}
}

// The headless-Linux path: no Secret Service, so the credential must land in a
// 0600 file and read back from it.
func TestFileFallbackWhenKeyringUnavailable(t *testing.T) {
	kr := newFakeKeyring()
	kr.unavailable = true
	s := newStore(t, kr)

	src, err := s.Set(auKey, token)
	if err != nil {
		t.Fatal(err)
	}
	if src != SourceFile {
		t.Fatalf("stored in %s, want the file", src)
	}

	info, err := os.Stat(s.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("credential file mode = %o, want 600", perm)
	}

	got, err := s.Get(auKey)
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != token || got.Source != SourceFile {
		t.Fatalf("Get = %+v", got)
	}
}

// A credential that was written to the file while the keyring was down must not
// survive a later keyring write. Leaving the stale copy behind would resurrect a
// revoked token the moment the keyring became unavailable again.
func TestKeyringWriteClearsAStaleFileCopy(t *testing.T) {
	kr := newFakeKeyring()
	kr.unavailable = true
	s := newStore(t, kr)
	if _, err := s.Set(auKey, "drift_old-token-value"); err != nil {
		t.Fatal(err)
	}

	kr.unavailable = false
	if _, err := s.Set(auKey, token); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(s.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "drift_old-token-value") {
		t.Fatalf("the stale file copy survived a keyring write:\n%s", raw)
	}
}

// DRIFT_TOKEN wins over both stores and is not context-scoped: an operator who
// exported it means it for whatever they run next.
func TestEnvTokenOverridesEverything(t *testing.T) {
	kr := newFakeKeyring()
	s := newStore(t, kr)
	if _, err := s.Set(auKey, token); err != nil {
		t.Fatal(err)
	}
	s.EnvToken = "drift_from-the-environment"

	got, err := s.Get(auKey)
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "drift_from-the-environment" || got.Source != SourceEnv {
		t.Fatalf("Get = %+v", got)
	}

	// Even with no context at all, which is the CI shape.
	got, err = s.Get(Key{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != SourceEnv {
		t.Fatalf("Get(\"\") = %+v", got)
	}
}

func TestGetWithoutAnyCredential(t *testing.T) {
	s := newStore(t, newFakeKeyring())
	_, err := s.Get(auKey)
	if !errors.Is(err, ErrNoCredential) {
		t.Fatalf("want ErrNoCredential, got %v", err)
	}
}

// Logout must clear BOTH backends. A logout that leaves a copy somewhere is
// worse than useless.
func TestDeleteClearsBothBackends(t *testing.T) {
	kr := newFakeKeyring()
	s := newStore(t, kr)

	// Put a copy in each: the file directly, the keyring through Set.
	kr.unavailable = true
	if _, err := s.Set(auKey, token); err != nil {
		t.Fatal(err)
	}
	kr.unavailable = false
	if err := kr.Set(KeyringService, auKey.storage(), token); err != nil {
		t.Fatal(err)
	}

	if err := s.Delete(auKey); err != nil {
		t.Fatal(err)
	}
	if _, err := kr.Get(KeyringService, auKey.storage()); err == nil {
		t.Fatal("the keyring entry survived logout")
	}
	if _, err := s.Get(auKey); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("the file entry survived logout: %v", err)
	}
}

// REGRESSION. `logout` used to report success and exit 0 while leaving a live
// credential in the keyring, in exactly the situation the file fallback exists
// for: the keyring is unreachable (headless box, SSH session, locked desktop),
// so the keyring delete fails while the file delete legitimately reports
// absence — and the old code treated that pair as "nothing was stored".
//
// The credential was still there on the next desktop session. Revocation is the
// control that matters most for a bearer token, so this must be an error loud
// enough to send the operator to the server's revoke button.
func TestDeleteFailsLoudlyWhenTheKeyringIsUnreachable(t *testing.T) {
	kr := newFakeKeyring()
	s := newStore(t, kr)

	// A credential in the keyring, put there while it worked.
	if _, err := s.Set(auKey, token); err != nil {
		t.Fatal(err)
	}
	// Now the keyring is gone. Nothing is in the file, so the file backend
	// honestly reports absence — the exact shape that used to read as success.
	kr.unavailable = true

	err := s.Delete(auKey)
	if err == nil {
		t.Fatal("logout reported success while the credential was still in the keyring")
	}
	if errors.Is(err, ErrNoCredential) {
		t.Fatalf("an unreachable keyring was reported as absence: %v", err)
	}
	if !strings.Contains(err.Error(), "keyring") {
		t.Fatalf("the error does not name the store that may still hold a copy: %v", err)
	}

	// And the credential really is still there, which is why this matters.
	kr.unavailable = false
	if _, err := kr.Get(KeyringService, auKey.storage()); err != nil {
		t.Fatalf("precondition: the credential should still be in the keyring: %v", err)
	}
}

// The mirror image: the file is unreadable while the keyring is fine.
func TestDeleteFailsLoudlyWhenTheFileCannotBeRewritten(t *testing.T) {
	kr := newFakeKeyring()
	kr.unavailable = true
	s := newStore(t, kr)
	if _, err := s.Set(auKey, token); err != nil {
		t.Fatal(err)
	}
	kr.unavailable = false

	// Corrupt the file so the delete path cannot parse it.
	if err := os.WriteFile(s.FilePath, []byte("{{{not yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := s.Delete(auKey)
	if err == nil || errors.Is(err, ErrNoCredential) {
		t.Fatalf("a broken credential file was not reported: %v", err)
	}
	if !strings.Contains(err.Error(), s.FilePath) {
		t.Fatalf("the error does not name the file: %v", err)
	}
}

func TestDeleteWhenNothingStored(t *testing.T) {
	s := newStore(t, newFakeKeyring())
	if err := s.Delete(auKey); err == nil {
		t.Fatal("expected an error when there is nothing to remove")
	}
}

func TestStoreRefusesAnEmptyContext(t *testing.T) {
	s := newStore(t, newFakeKeyring())
	if _, err := s.Set(Key{}, token); err == nil {
		t.Fatal("expected storing without a context to fail")
	}
}

// Describe is the ONLY sanctioned way to say something about a credential. It
// must identify one without revealing it.
func TestDescribeNeverRevealsTheToken(t *testing.T) {
	got := Describe(token)
	if strings.Contains(token, strings.TrimSuffix(strings.TrimPrefix(got, "drift_"), "…")) == false {
		t.Fatalf("Describe produced something unrelated to the token: %q", got)
	}
	if len(got) >= len(token) {
		t.Fatalf("Describe returned %d of %d characters", len(got), len(token))
	}
	// The body after the prefix must be at most the 6-character sample.
	body := strings.TrimSuffix(strings.TrimPrefix(got, "drift_"), "…")
	if len(body) > 6 {
		t.Fatalf("Describe revealed %d characters of the secret", len(body))
	}
	if Describe("") != "(none)" {
		t.Fatalf("Describe(\"\") = %q", Describe(""))
	}
	if Describe("short") != "(malformed token)" {
		t.Fatalf("Describe(short) = %q", Describe("short"))
	}
}

// Two contexts must not share a credential.
func TestCredentialsAreContextScoped(t *testing.T) {
	kr := newFakeKeyring()
	kr.unavailable = true // exercise the file backend, which keys by context
	s := newStore(t, kr)

	if _, err := s.Set(auKey, "drift_au-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set(enKey, "drift_en-token"); err != nil {
		t.Fatal(err)
	}

	au, _ := s.Get(auKey)
	en, _ := s.Get(enKey)
	if au.Token != "drift_au-token" || en.Token != "drift_en-token" {
		t.Fatalf("contexts crossed: au=%q en=%q", au.Token, en.Token)
	}

	// Deleting needs a reachable keyring, or Delete correctly refuses to claim
	// the credential is gone. That refusal has its own test.
	kr.unavailable = false
	if err := s.Delete(auKey); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get(enKey); err != nil || got.Token != "drift_en-token" {
		t.Fatalf("removing au disturbed en: %+v %v", got, err)
	}
}

// REGRESSION. `Set` cleared the stale FILE copy when the keyring won, but not
// the stale KEYRING entry when the file won — and `Get` reads the keyring
// first, so the old credential did not merely linger, it took precedence.
//
// The sequence that bit: log in at a desk (keyring), rotate the credential over
// SSH where there is no keyring (file), come back to the desk, and the CLI
// silently resumes the superseded token.
// REGRESSION. `Set` cleared the stale FILE copy when the keyring won, but not
// the stale KEYRING entry when the file won — and `Get` reads the keyring
// first, so the old credential did not merely linger, it took precedence.
//
// The sequence that bit: log in at a desk (keyring), rotate over SSH where
// there is no keyring (file), come back to the desk.
//
// Over SSH the keyring cannot be READ either, so the file cannot record which
// keyring value it superseded. That is the one case with no evidence available
// in either direction, and it is reported rather than guessed: silently
// preferring the keyring is the superseded-token bug, and silently preferring
// the file lets the weaker store promote itself. One command settles it.
func TestRotationWithAnUnreadableKeyringIsReportedNotGuessed(t *testing.T) {
	kr := newFakeKeyring()
	s := newStore(t, kr)

	if _, err := s.Set(auKey, "drift_old-desktop-token"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(auKey); got.Source != SourceKeyring {
		t.Fatalf("precondition: expected the keyring, got %s", got.Source)
	}

	// Over SSH: the keyring cannot be read or written.
	kr.unavailable = true
	src, err := s.Set(auKey, "drift_new-rotated-token")
	if err != nil {
		t.Fatal(err)
	}
	if src != SourceFile {
		t.Fatalf("stored in %s, want the file", src)
	}
	// While the keyring is unreachable the file is the only credential there is,
	// so the rotation takes effect immediately where it was performed.
	if got, err := s.Get(auKey); err != nil || got.Token != "drift_new-rotated-token" {
		t.Fatalf("the rotation did not take effect over SSH: %+v %v", got, err)
	}

	// Back at the desk: two different credentials, no evidence.
	kr.unavailable = false
	_, err = s.Get(auKey)
	if !errors.Is(err, ErrAmbiguousCredential) {
		t.Fatalf("want ErrAmbiguousCredential, got %v", err)
	}
	// Whichever way it had been resolved silently would have been wrong; what
	// matters is that the superseded token is not returned.
	if got, _ := s.Get(auKey); got.Token == "drift_old-desktop-token" {
		t.Fatal("the superseded token was returned")
	}

	// Logging in again settles it, and the keyring is authoritative once more.
	if _, err := s.Set(auKey, "drift_new-rotated-token"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(auKey)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != SourceKeyring || got.Token != "drift_new-rotated-token" {
		t.Fatalf("login did not settle the conflict: %+v", got)
	}
}

// ...and `logout` works in the ambiguous state, clearing both copies. It must,
// or the only other way out would be editing files by hand.
func TestLogoutResolvesAnAmbiguousState(t *testing.T) {
	kr := newFakeKeyring()
	s := newStore(t, kr)
	if _, err := s.Set(auKey, "drift_old-desktop-token"); err != nil {
		t.Fatal(err)
	}
	kr.unavailable = true
	if _, err := s.Set(auKey, "drift_new-rotated-token"); err != nil {
		t.Fatal(err)
	}
	kr.unavailable = false
	if _, err := s.Get(auKey); !errors.Is(err, ErrAmbiguousCredential) {
		t.Fatalf("precondition: expected ambiguity, got %v", err)
	}

	if err := s.Delete(auKey); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(auKey); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("logout left something behind: %v", err)
	}
}

// When the keyring can be READ but not written, the file records the digest of
// the exact token it supersedes — evidence, not an assertion — and outranks it
// automatically with no operator involvement.
func TestFileWriteSupersedesAnUnclearableKeyringEntry(t *testing.T) {
	kr := newFakeKeyring()
	s := newStore(t, kr)
	if _, err := s.Set(auKey, "drift_old-desktop-token"); err != nil {
		t.Fatal(err)
	}

	// The keyring is reachable for reads but refuses writes and deletes — a
	// locked collection, or a policy that forbids modification.
	kr.failDelete = errors.New("keyring locked")
	kr.failSet = errors.New("keyring locked")
	src, err := s.Set(auKey, "drift_new-rotated-token")
	if err != nil {
		t.Fatal(err)
	}
	if src != SourceFile {
		t.Fatalf("stored in %s, want the file", src)
	}

	got, err := s.Get(auKey)
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "drift_new-rotated-token" || got.Source != SourceFile {
		t.Fatalf("the stale keyring entry still won: %+v", got)
	}

	// ...and once the keyring accepts a write again, it becomes authoritative
	// and the marker is cleared with the file entry.
	kr.failSet, kr.failDelete = nil, nil
	if _, err := s.Set(auKey, "drift_newest"); err != nil {
		t.Fatal(err)
	}
	got, err = s.Get(auKey)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != SourceKeyring || got.Token != "drift_newest" {
		t.Fatalf("the keyring did not regain authority: %+v", got)
	}
}

// REGRESSION. The temp file used to be a fixed, guessable `<path>.tmp`, and
// os.WriteFile ignores its mode argument when the file already exists — so a
// pre-existing 0666 temp file produced a 0666 credentials file, and a symlink
// there sent the token wherever it pointed.
func TestWriteDoesNotInheritAPlantedTempFilesMode(t *testing.T) {
	kr := newFakeKeyring()
	kr.unavailable = true
	s := newStore(t, kr)

	planted := s.FilePath + ".tmp"
	if err := os.WriteFile(planted, []byte("planted"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(planted, 0o666); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Set(auKey, token); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("credential file mode = %o, want 600", perm)
	}
	// And no temp file is left holding the token in the clear.
	entries, err := os.ReadDir(filepath.Dir(s.FilePath))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".credentials-") {
			t.Fatalf("a temp file survived the write: %s", e.Name())
		}
	}
}

// A symlink at the old fixed temp path must not receive the token.
func TestWriteDoesNotFollowASymlinkAtTheTempPath(t *testing.T) {
	kr := newFakeKeyring()
	kr.unavailable = true
	s := newStore(t, kr)

	target := filepath.Join(t.TempDir(), "elsewhere.yaml")
	if err := os.Symlink(target, s.FilePath+".tmp"); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := s.Set(auKey, token); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(target); err == nil && strings.Contains(string(raw), token) {
		t.Fatalf("the token was written through a symlink to %s", target)
	}
}

// A pre-existing loose config directory is tightened rather than trusted.
func TestWriteTightensALooseDirectory(t *testing.T) {
	kr := newFakeKeyring()
	kr.unavailable = true
	dir := t.TempDir()
	s := &Store{Backend: kr, FilePath: filepath.Join(dir, "sub", "credentials.yaml")}
	if err := os.MkdirAll(filepath.Dir(s.FilePath), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(s.FilePath), 0o777); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Set(auKey, token); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Dir(s.FilePath))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("credential directory left at mode %o", perm)
	}
}

// Describe must not dress an unrecognised token up as a drift credential.
func TestDescribeDoesNotFakeThePrefix(t *testing.T) {
	got := Describe("ghp_averylongtokenvalue")
	if strings.HasPrefix(got, "drift_") {
		t.Fatalf("Describe invented a drift_ prefix: %q", got)
	}
	if !strings.Contains(got, "unrecognised") {
		t.Fatalf("Describe did not flag the format: %q", got)
	}
}

// MEDIUM regression. The marker used to be a bare "the file wins" flag, so a
// `credentials.yaml` restored from a backup or dragged in by a file-sync tool
// could promote itself over a live keyring entry — the operator's commands
// running under a credential someone else chose.
//
// The marker is now the digest of the keyring token it supersedes, which a
// planted file cannot produce without already knowing that token.
func TestAPlantedFileCannotPromoteItselfOverTheKeyring(t *testing.T) {
	kr := newFakeKeyring()
	s := newStore(t, kr)
	if _, err := s.Set(auKey, "drift_live-keyring-token"); err != nil {
		t.Fatal(err)
	}

	// A planted file claiming precedence with a forged marker.
	planted := "credentials:\n    " + auKey.storage() + ": drift_attacker-chosen\n" +
		"supersedes-keyring:\n    " + auKey.storage() + ": \"0000000000000000000000000000000000000000000000000000000000000000\"\n"
	if err := os.WriteFile(s.FilePath, []byte(planted), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(auKey)
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "drift_live-keyring-token" || got.Source != SourceKeyring {
		t.Fatalf("a planted file overrode the keyring: %+v", got)
	}

	// The bare-flag form of the same claim is equally powerless.
	planted = "credentials:\n    " + auKey.storage() + ": drift_attacker-chosen\n" +
		"supersedes-keyring:\n    " + auKey.storage() + ": true\n"
	if err := os.WriteFile(s.FilePath, []byte(planted), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = s.Get(auKey)
	if err != nil && !errors.Is(err, ErrAmbiguousCredential) {
		t.Fatal(err)
	}
	if got.Token == "drift_attacker-chosen" {
		t.Fatalf("a planted file overrode the keyring: %+v", got)
	}
}

// HIGH regression, F4 as re-reported. `Set` discarded the result of the file
// clear, so an unwritable config directory left the file entry AND its marker in
// place while login exited 0 saying "stored in the OS keyring".
func TestKeyringWriteReportsAFileClearFailure(t *testing.T) {
	kr := newFakeKeyring()
	kr.unavailable = true
	s := newStore(t, kr)

	// A credential in the file, from a headless login.
	if _, err := s.Set(auKey, "drift_old-file-token"); err != nil {
		t.Fatal(err)
	}

	// Now the keyring works, but the file cannot be rewritten.
	kr.unavailable = false
	if err := os.WriteFile(s.FilePath, []byte("{{{not yaml"), 0o600); err != nil {
		t.Fatal(err)
	}

	src, err := s.Set(auKey, "drift_new-token")
	if src != SourceKeyring {
		t.Fatalf("stored in %s, want the keyring", src)
	}
	var stale *StaleCopyError
	if !errors.As(err, &stale) {
		t.Fatalf("the surviving file copy was not reported: %v", err)
	}
	if !strings.Contains(stale.Error(), s.FilePath) {
		t.Fatalf("the report does not name the file: %v", stale)
	}
}

// MEDIUM regression. `writeFile` is a read-modify-write over a file holding
// EVERY context, and was unserialised: 24 concurrent writers for 24 distinct
// contexts left 6 survivors.
func TestConcurrentWritesDoNotLoseCredentials(t *testing.T) {
	kr := newFakeKeyring()
	kr.unavailable = true // force every write onto the shared file
	s := newStore(t, kr)

	const n = 24
	keys := make([]Key, n)
	for i := range keys {
		keys[i] = NewKey(fmt.Sprintf("ctx-%02d", i), fmt.Sprintf("https://drift-%02d.example.com", i))
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range keys {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.Set(keys[i], fmt.Sprintf("drift_token-%02d", i))
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d failed: %v", i, err)
		}
	}
	for i, k := range keys {
		got, err := s.Get(k)
		if err != nil {
			t.Fatalf("credential %d was lost: %v", i, err)
		}
		if want := fmt.Sprintf("drift_token-%02d", i); got.Token != want {
			t.Fatalf("credential %d = %q, want %q", i, got.Token, want)
		}
	}
}

// Concurrent deletes must not lose the OTHER contexts either.
func TestConcurrentDeletesDoNotLoseCredentials(t *testing.T) {
	kr := newFakeKeyring()
	kr.unavailable = true
	s := newStore(t, kr)

	const n = 12
	keys := make([]Key, n)
	for i := range keys {
		keys[i] = NewKey(fmt.Sprintf("ctx-%02d", i), fmt.Sprintf("https://drift-%02d.example.com", i))
		if _, err := s.Set(keys[i], fmt.Sprintf("drift_token-%02d", i)); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < n; i += 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = s.Delete(keys[i])
		}(i)
	}
	wg.Wait()

	for i := 1; i < n; i += 2 {
		if _, err := s.Get(keys[i]); err != nil {
			t.Fatalf("credential %d was lost by a concurrent delete: %v", i, err)
		}
	}
}

// LOW regression. A malformed `supersedes-keyring` used to fail the whole parse,
// so one bad value made every credential for every context unreadable. It is a
// hint; it must degrade to "no hint".
func TestMalformedSupersedeMarkerDoesNotBrickTheStore(t *testing.T) {
	kr := newFakeKeyring()
	kr.unavailable = true
	s := newStore(t, kr)
	if _, err := s.Set(auKey, token); err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{
		"supersedes-keyring: not-a-list\n",
		"supersedes-keyring:\n    - au\n",
		"supersedes-keyring: 42\n",
		"supersedes-keyring:\n    au: [1, 2]\n",
	} {
		body := "credentials:\n    " + auKey.storage() + ": " + token + "\n" + bad
		if err := os.WriteFile(s.FilePath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(auKey)
		if err != nil {
			t.Fatalf("%q made the store unreadable: %v", bad, err)
		}
		if got.Token != token {
			t.Fatalf("%q lost the credential: %+v", bad, got)
		}
	}
}

// --- composite keying -------------------------------------------------------

// Two config files that both call a context `prod` must not share an entry:
// that is the storage-layer form of the rule that a credential travels only to
// the address its own context vouches for.
func TestSameNameDifferentEndpointsAreSeparateCredentials(t *testing.T) {
	kr := newFakeKeyring()
	s := newStore(t, kr)

	a := NewKey("prod", "https://drift.a.example.com")
	b := NewKey("prod", "https://drift.b.example.com")
	if a.storage() == b.storage() {
		t.Fatal("the two keys collide")
	}

	if _, err := s.Set(a, "drift_token-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set(b, "drift_token-b"); err != nil {
		t.Fatal(err)
	}
	ga, _ := s.Get(a)
	gb, _ := s.Get(b)
	if ga.Token != "drift_token-a" || gb.Token != "drift_token-b" {
		t.Fatalf("entries crossed: a=%q b=%q", ga.Token, gb.Token)
	}

	// Removing one leaves the other.
	if err := s.Delete(a); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get(b); err != nil || got.Token != "drift_token-b" {
		t.Fatalf("removing a disturbed b: %+v %v", got, err)
	}
}

// The trailing-slash spelling the credential gate deliberately accepts must not
// become a second entry.
func TestEndpointSpellingsShareOneKey(t *testing.T) {
	for _, spelling := range []string{
		"https://drift.au.example.com",
		"https://drift.au.example.com/",
		"  https://drift.au.example.com  ",
	} {
		if got := NewKey("au", spelling).storage(); got != auKey.storage() {
			t.Fatalf("%q keyed as %q, want %q", spelling, got, auKey.storage())
		}
	}
}

// MEDIUM regression. The keyring is GLOBAL to the user account, so a
// bare-context-name entry is shared by every config directory using that name.
// Falling back to it handed whichever directory read first a credential minted
// for a different deployment, and then migrated it under that directory's key so
// the rightful owner found nothing — reintroducing, for exactly the population
// the migration was for, the sharing the composite key exists to end.
//
// The fallback is refused. A forced re-login is the smaller cost.
func TestLegacyKeyringEntryIsNotUsedAsACredential(t *testing.T) {
	kr := newFakeKeyring()

	// Two config directories, both calling their context `prod`, pointing at
	// different deployments — and one legacy entry from an older build.
	a := &Store{Backend: kr, FilePath: filepath.Join(t.TempDir(), "credentials.yaml")}
	b := &Store{Backend: kr, FilePath: filepath.Join(t.TempDir(), "credentials.yaml")}
	keyA := NewKey("prod", "https://drift.a.example.com")
	keyB := NewKey("prod", "https://drift.b.example.com")
	if err := kr.Set(KeyringService, keyA.legacy(), "drift_MINTED-FOR-A"); err != nil {
		t.Fatal(err)
	}

	// Neither may present it as a credential, whichever reads first.
	if _, err := b.Get(keyB); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("B was handed a credential minted for A: %v", err)
	}
	if _, err := a.Get(keyA); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("the legacy entry was used without proof of which endpoint it belongs to: %v", err)
	}
	// ...and it is not silently migrated out from under the other directory.
	if v, err := kr.Get(KeyringService, keyA.legacy()); err != nil || v != "drift_MINTED-FOR-A" {
		t.Fatalf("the legacy entry was consumed: %q %v", v, err)
	}

	// The presence IS detectable, so the CLI can explain the apparent logout
	// rather than leaving the user to guess.
	if !a.HasLegacyKeyringEntry(keyA) {
		t.Fatal("the legacy entry is invisible; the CLI cannot explain the re-login")
	}
	if a.HasLegacyKeyringEntry(NewKey("other", "https://drift.a.example.com")) {
		t.Fatal("HasLegacyKeyringEntry answered for a context with no legacy entry")
	}

	// And logout still clears it, or it would be an unreachable live token.
	if err := a.Delete(keyA); err != nil {
		t.Fatal(err)
	}
	if _, err := kr.Get(KeyringService, keyA.legacy()); !errors.Is(err, ErrNoCredential) {
		t.Fatal("the legacy entry survived logout")
	}
}

func TestLegacyKeyedFileCredentialIsFoundAndMigrated(t *testing.T) {
	kr := newFakeKeyring()
	kr.unavailable = true
	s := newStore(t, kr)

	body := "credentials:\n    au: " + token + "\n"
	if err := os.WriteFile(s.FilePath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(auKey)
	if err != nil || got.Token != token {
		t.Fatalf("a legacy file credential was not found: %+v %v", got, err)
	}

	cf, err := s.loadFile()
	if err != nil {
		t.Fatal(err)
	}
	if cf.Credentials[auKey.storage()] != token {
		t.Fatalf("not migrated: %#v", cf.Credentials)
	}
	if _, ok := cf.Credentials[auKey.legacy()]; ok {
		t.Fatal("the legacy file entry survived migration")
	}
}

// The FILE keeps its legacy fallback: a credentials file belongs to one config
// directory, so an entry there can only have been written by this
// configuration and there is nothing to confuse it with.

// Delete must try BOTH keys, or logout silently leaves a legacy entry live —
// which is the F3 silent-success bug wearing a different hat.
func TestDeleteClearsLegacyKeysToo(t *testing.T) {
	kr := newFakeKeyring()
	s := newStore(t, kr)
	if err := kr.Set(KeyringService, auKey.legacy(), token); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(auKey); err != nil {
		t.Fatal(err)
	}
	if _, err := kr.Get(KeyringService, auKey.legacy()); !errors.Is(err, ErrNoCredential) {
		t.Fatal("the legacy entry survived logout")
	}
}
