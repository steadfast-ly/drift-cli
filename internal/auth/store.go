// Package auth stores and retrieves the CLI credential.
//
// Three sources, in precedence order:
//
//  1. `DRIFT_TOKEN` — bypasses storage entirely and suppresses interactive
//     login. This is the CI path: a runner has no keyring and no operator to
//     paste anything, and a CLI that tried to prompt there would hang a build.
//  2. The OS keyring (`zalando/go-keyring`).
//  3. A 0600 file under the config directory — the headless-Linux fallback,
//     where there is no D-Bus Secret Service to talk to.
//
// The token is a bearer credential: whoever holds it is the user. Nothing in
// this package logs, prints or formats a token value, and `Describe` exists so
// that callers that want to SAY something about a credential have a
// non-revealing way to do it.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/steadfast/drift-cli/internal/config"
	"github.com/zalando/go-keyring"
	"gopkg.in/yaml.v3"
)

// KeyringService is the service name credentials are filed under in the OS
// keyring. Constant, because a rename orphans every stored credential.
const KeyringService = "drift-cli"

// Entries are keyed by CONTEXT NAME **plus ENDPOINT**, in both backends.
//
// The rule the whole credential design rests on is that a stored credential
// travels only to the address its own context vouches for. Keying on the name
// alone broke that rule at the storage layer: two config files on the same
// machine using `prod` for different deployments shared one entry, so a
// credential minted for one server was handed to the other — the very thing
// `config.Resolved.CredentialContext` refuses to do at the request layer.
//
// The objection to endpoint keying is that moving a deployment's hostname
// orphans the credential. That is correct and it is the RIGHT behaviour: a
// credential issued by the server at one address has no standing at another,
// and the remedy — log in again — is one command. An orphaned entry is also
// reachable: `context add` with a changed endpoint clears the old-keyed entry,
// and `Delete` always tries both keys.
//
// The endpoint is hashed rather than embedded whole. macOS Keychain and Windows
// Credential Manager both constrain the length and character set of an account
// name, and a URL is neither short nor restricted. The cost is that the entry
// reads as `au@1f3c…` in Keychain Access rather than naming the server, which
// makes manual revocation harder; the context name is kept in the clear so the
// entry can still be recognised.
const keySeparator = "@"

// endpointKeyLength is how much of the endpoint digest goes into the key. 16 hex
// characters is 64 bits — far beyond collision range for the handful of
// endpoints one operator configures, and short enough to stay well inside every
// platform's limits.
const endpointKeyLength = 16

// Key identifies one stored credential: a context name and the endpoint that
// context points at.
type Key struct {
	Context  string
	Endpoint string
}

// NewKey builds a key, canonicalising the endpoint exactly as the config layer
// does so that `https://x` and `https://x/` are one entry rather than two.
func NewKey(context, endpoint string) Key {
	return Key{Context: context, Endpoint: config.NormalizeEndpoint(endpoint)}
}

func (k Key) ok() bool { return k.Context != "" && k.Endpoint != "" }

// storage is the composite key used in both backends.
func (k Key) storage() string {
	sum := sha256.Sum256([]byte(config.NormalizeEndpoint(k.Endpoint)))
	return k.Context + keySeparator + hex.EncodeToString(sum[:])[:endpointKeyLength]
}

// legacy is the pre-composite key: the bare context name.
//
// It is still WRITTEN to by nothing and still DELETED by everything — `Delete`
// always tries it, because clearing only the composite key would leave a live
// credential behind while reporting success.
//
// Reads are asymmetric between the two backends, deliberately:
//
//   - The FILE falls back to it and migrates. A credentials file belongs to one
//     config directory, so a legacy entry there can only have been written by
//     this configuration and there is nothing to confuse it with.
//   - The KEYRING does NOT. The keyring is global to the user account, so a bare
//     `prod` entry is shared by every config directory that uses that name — and
//     falling back to it hands whichever directory reads first a credential
//     minted for a different deployment, then migrates it under that directory's
//     key so the rightful owner finds nothing. That is exactly the sharing the
//     composite key exists to end, reintroduced for the one population the
//     migration was for. A forced re-login is the smaller cost, and it is not
//     silent: `HasLegacyKeyringEntry` lets the CLI say what happened.
func (k Key) legacy() string { return k.Context }

// ErrNoCredential is returned when no credential exists for a key.
var ErrNoCredential = errors.New("no credential stored")

// ErrAmbiguousCredential is returned when both backends hold DIFFERENT
// credentials and there is no evidence for which is current.
//
// It arises from one sequence: a credential was written to the file while the
// keyring could not be read at all, so the file could not record which keyring
// value it superseded — and the keyring is now readable and holds something
// else. Rather than pick, which would be either a silently superseded token or
// a file granting itself precedence over the stronger store, the ambiguity is
// reported and one command settles it.
var ErrAmbiguousCredential = errors.New("two different credentials are stored and drift cannot tell which is current")

// supersedeUnknown marks a file entry written while the keyring was unreadable.
// Distinguished from a digest because it is an ASSERTION rather than evidence,
// and the two must not be treated alike.
const supersedeUnknown = "unknown"

// Source identifies where a credential came from. Reported by `auth status` and
// `doctor`, because "it works but I do not know which token it used" is a real
// support burden.
type Source string

const (
	SourceEnv     Source = "environment (DRIFT_TOKEN)"
	SourceKeyring Source = "OS keyring"
	SourceFile    Source = "file"
	SourceNone    Source = "none"
)

// Backend is the keyring operations this package needs. An interface so tests
// can substitute a fake without a D-Bus session.
//
// Implementations MUST distinguish the two failures, by returning an error that
// satisfies `errors.Is(err, ErrNoCredential)` when the secret is simply not
// there and any other error when the store could not be reached. `Delete`
// depends on the difference: "there was nothing to remove" is a success, and
// "the keyring did not answer" means a live credential may remain.
type Backend interface {
	Get(service, user string) (string, error)
	Set(service, user, password string) error
	Delete(service, user string) error
}

type osKeyring struct{}

func (osKeyring) Get(service, user string) (string, error) {
	v, err := keyring.Get(service, user)
	return v, translate(err)
}

func (osKeyring) Set(service, user, password string) error {
	return translate(keyring.Set(service, user, password))
}

func (osKeyring) Delete(service, user string) error {
	return translate(keyring.Delete(service, user))
}

// translate maps the library's not-found sentinel onto this package's, so the
// rest of the code can reason about absence without importing go-keyring.
func translate(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("%w", ErrNoCredential)
	}
	return err
}

// Store reads and writes credentials.
type Store struct {
	Backend Backend
	// FilePath is the 0600 fallback. Empty means "derive from the config dir".
	FilePath string
	// EnvToken is the DRIFT_TOKEN value. Injected rather than read from the
	// process environment so tests do not have to mutate global state.
	EnvToken string
}

// NewStore builds a store wired to the real OS keyring, the real config
// directory and the real environment.
func NewStore(configDir string) *Store {
	return &Store{
		Backend:  osKeyring{},
		FilePath: filepath.Join(configDir, "credentials.yaml"),
		EnvToken: os.Getenv("DRIFT_TOKEN"),
	}
}

// Credential is a resolved credential and the story of where it came from.
type Credential struct {
	Token  string
	Source Source
}

// StaleCopyError reports that a credential was stored, but a superseded copy of
// a DIFFERENT credential could not be removed from the other backend.
//
// Its own type because the outcome is neither success nor a failed login: the
// new credential is in place, and there is a leftover that may outrank it or may
// simply be a revoked token sitting on disk. Both need the operator, so it is
// reported rather than logged.
type StaleCopyError struct {
	Where string
	Err   error
}

func (e *StaleCopyError) Error() string {
	return fmt.Sprintf("a superseded credential could not be removed from %s: %v", e.Where, e.Err)
}

func (e *StaleCopyError) Unwrap() error { return e.Err }

// Describe renders a credential safely: never the value, only enough to tell
// two credentials apart. `drift_` tokens are 32 bytes of base64url, so the
// first six characters after the prefix identify one without being usefully
// guessable.
func Describe(token string) string {
	if token == "" {
		return "(none)"
	}
	const prefix = "drift_"
	// The prefix is echoed back only when it was actually there. Printing
	// `drift_` in front of a token that lacks it makes a pasted-the-wrong-thing
	// mistake — a session cookie, a GitHub PAT — look like a well-formed drift
	// credential, which is the opposite of what this function is for.
	body, hadPrefix := strings.CutPrefix(token, prefix)
	visible := 6
	if len(body) < visible {
		// A token this short is malformed; say so rather than echoing it.
		return "(malformed token)"
	}
	if !hadPrefix {
		return "(unrecognised format) " + body[:visible] + "…"
	}
	return prefix + body[:visible] + "…"
}

// Get resolves the credential for a key.
//
// The environment override is checked first and is not key-scoped: an operator
// who exported DRIFT_TOKEN means it for whatever they run next.
func (s *Store) Get(k Key) (Credential, error) {
	if s.EnvToken != "" {
		return Credential{Token: s.EnvToken, Source: SourceEnv}, nil
	}
	if !k.ok() {
		return Credential{Source: SourceNone}, ErrNoCredential
	}

	cf, err := s.loadFile()
	if err != nil {
		return Credential{Source: SourceNone}, err
	}
	fileToken, fileLegacy := cf.lookup(k)
	keyToken := s.keyringLookup(k)

	// A legacy FILE entry is rewritten under the composite key, so an upgrade is
	// a one-time migration rather than a permanent fallback.
	if fileLegacy {
		s.migrateFile(k)
	}

	switch cf.precedence(k, keyToken, fileToken) {
	case preferFile:
		return Credential{Token: fileToken, Source: SourceFile}, nil
	case preferKeyring:
		return Credential{Token: keyToken, Source: SourceKeyring}, nil
	case ambiguous:
		return Credential{Source: SourceNone}, fmt.Errorf(
			"%w for context %q at %s: one is in the OS keyring, one in %s",
			ErrAmbiguousCredential, k.Context, k.Endpoint, s.FilePath)
	}
	return Credential{Source: SourceNone}, ErrNoCredential
}

// keyringLookup reads the keyring under the composite key ONLY. See Key.legacy
// for why there is no fallback here.
func (s *Store) keyringLookup(k Key) string {
	if s.Backend == nil {
		return ""
	}
	if tok, err := s.Backend.Get(KeyringService, k.storage()); err == nil && tok != "" {
		return tok
	}
	return ""
}

// HasLegacyKeyringEntry reports whether a credential from an earlier build is
// filed under the bare context name.
//
// It is not USED as a credential — it cannot be attributed to an endpoint — but
// its presence is why a working install can appear logged out after an upgrade,
// so the CLI says so instead of leaving the user to guess.
func (s *Store) HasLegacyKeyringEntry(k Key) bool {
	if s.Backend == nil || k.Context == "" {
		return false
	}
	tok, err := s.Backend.Get(KeyringService, k.legacy())
	return err == nil && tok != ""
}

// migrateFile rewrites a legacy-keyed file entry under the composite key.
//
// Best effort: a migration that cannot complete must not fail the command the
// user actually ran, and `Delete` tries both keys forever, so a leftover legacy
// entry stays reachable rather than becoming an orphan.
func (s *Store) migrateFile(k Key) {
	_ = s.withLock(func() error {
		cf, err := s.loadFile()
		if err != nil {
			return err
		}
		if cf.Credentials[k.legacy()] == "" {
			return nil
		}
		if cf.Credentials[k.storage()] == "" {
			cf.Credentials[k.storage()] = cf.Credentials[k.legacy()]
		}
		if h, ok := cf.SupersedesKeyring[k.legacy()]; ok {
			if _, exists := cf.SupersedesKeyring[k.storage()]; !exists {
				cf.SupersedesKeyring[k.storage()] = h
			}
		}
		delete(cf.Credentials, k.legacy())
		delete(cf.SupersedesKeyring, k.legacy())
		return s.saveFile(cf)
	})
}

// Set stores a credential, clearing the other backend's copy.
//
// Whichever backend a credential lands in, the OTHER one is cleared. A
// superseded copy in the store that is not being read today is a credential
// that comes back to life the moment availability flips, and both directions
// happen in practice:
//
//   - keyring wins, stale file left behind: the box goes headless, `Get` falls
//     to the file, and a revoked token is live again.
//   - file wins (keyring was down), stale keyring entry left behind: log in on a
//     desktop, rotate over SSH, come back, and `Get` prefers the OLD token.
//
// When a clear FAILS, the leftover is reported as a StaleCopyError rather than
// discarded. Discarding the file-clear failure was a live bug: an unwritable
// config directory left the old file entry and its supersede marker in place,
// login said "stored in the OS keyring" and exited 0, and every subsequent
// command used the superseded token with no diagnostic anywhere.
func (s *Store) Set(k Key, token string) (Source, error) {
	if !k.ok() {
		return SourceNone, errors.New("cannot store a credential without a context and an endpoint")
	}

	// The keyring write happens WITHOUT the file lock. The lock guards the
	// credential file and nothing else, so taking it first meant a read-only
	// config directory failed the whole operation with "lock credential file:
	// permission denied" even though the keyring — the store that actually
	// works, and the preferred one — was available. Fail-closed is right for
	// writing the file; it is not right for a store the file has no bearing on.
	if s.Backend != nil {
		if err := s.Backend.Set(KeyringService, k.storage(), token); err == nil {
			return SourceKeyring, s.clearOthersAfterKeyringWrite(k)
		}
	}

	// The file is the destination now, so the lock is required and a failure to
	// take it is fatal.
	var src Source
	var setErr error
	if err := s.withLock(func() error {
		src, setErr = s.setFileLocked(k, token)
		return nil
	}); err != nil {
		return SourceNone, err
	}
	return src, setErr
}

// clearOthersAfterKeyringWrite removes copies the keyring write has superseded.
//
// Both leftovers are reported rather than discarded. The file copy carries a
// supersede marker that can make it outrank the keyring, and a surviving legacy
// keyring entry is invisible to THIS configuration but still visible to another
// config directory using the same context name.
func (s *Store) clearOthersAfterKeyringWrite(k Key) error {
	var stale []string
	if err := s.Backend.Delete(KeyringService, k.legacy()); err != nil && !errors.Is(err, ErrNoCredential) {
		stale = append(stale, fmt.Sprintf("the OS keyring, under the pre-upgrade name %q (%v)", k.legacy(), err))
	}

	// Only take the lock if there is a file to rewrite: otherwise a read-only
	// config directory would turn a successful keyring login into a failure.
	if s.fileWorkNeeded() {
		err := s.withLock(func() error { return s.deleteFileLocked(k) })
		if err != nil && !errors.Is(err, ErrNoCredential) {
			stale = append(stale, fmt.Sprintf("the credential file %s (%v)", s.FilePath, err))
		}
	}

	if len(stale) == 0 {
		return nil
	}
	return &StaleCopyError{Where: strings.Join(stale, " and ")}
}

// setFileLocked writes the credential to the file. Called with the lock held.
func (s *Store) setFileLocked(k Key, token string) (Source, error) {
	// The keyring would not take it, so the file wins. Read what the keyring
	// currently holds BEFORE trying to clear it: if the clear fails, that value
	// is what the marker has to name.
	superseded := s.keyringLookup(k)
	cleared := s.Backend == nil
	var clearErr error
	if s.Backend != nil {
		e1 := s.Backend.Delete(KeyringService, k.storage())
		e2 := s.Backend.Delete(KeyringService, k.legacy())
		ok1 := e1 == nil || errors.Is(e1, ErrNoCredential)
		ok2 := e2 == nil || errors.Is(e2, ErrNoCredential)
		cleared = ok1 && ok2
		if !ok1 {
			clearErr = e1
		} else if !ok2 {
			clearErr = e2
		}
	}

	// The marker records EVIDENCE where there is any, and says so plainly where
	// there is not. Claiming precedence without evidence is what would make the
	// weaker store able to promote itself.
	marker := ""
	switch {
	case cleared:
		// Nothing left in the keyring to outrank.
	case superseded != "":
		marker = hashToken(superseded)
	default:
		// The keyring refused even a read, so what it holds is unknown.
		marker = supersedeUnknown
	}
	_ = clearErr
	if err := s.writeFileLocked(k, token, marker); err != nil {
		return SourceNone, err
	}
	return SourceFile, nil
}

// Delete removes a credential from both backends, under both keys.
//
// Absence is reported ONLY when every store agrees the credential is not there.
// Any other failure is an error, because revocation is the control that matters
// most for a bearer token and "logged out" has to mean it.
//
// Both keys, always: after the move to composite keying, clearing only the new
// key would leave a legacy entry live while reporting success — reintroducing
// exactly the silent-failure bug this shape exists to prevent.
func (s *Store) Delete(k Key) error {
	if k.Context == "" {
		return ErrNoCredential
	}

	type outcome struct {
		where string
		err   error
	}
	var results []outcome

	// Keyring first and unlocked, for the same reason as Set: an unwritable
	// config directory must not stop a credential being revoked from the store
	// that has it. Logout is the control that matters most here.
	if s.Backend != nil {
		if k.ok() {
			results = append(results, outcome{"the OS keyring", s.Backend.Delete(KeyringService, k.storage())})
		}
		results = append(results, outcome{"the OS keyring", s.Backend.Delete(KeyringService, k.legacy())})
	} else {
		results = append(results, outcome{"the OS keyring", ErrNoCredential})
	}

	if s.fileWorkNeeded() {
		var ferr error
		if lerr := s.withLock(func() error {
			ferr = s.deleteFileLocked(k)
			return nil
		}); lerr != nil {
			ferr = lerr
		}
		results = append(results, outcome{"the credential file " + s.FilePath, ferr})
	} else {
		results = append(results, outcome{"the credential file " + s.FilePath, ErrNoCredential})
	}

	var stuck []string
	allAbsent := true
	for _, r := range results {
		switch {
		case r.err == nil:
			allAbsent = false
		case errors.Is(r.err, ErrNoCredential):
			// Nothing there; nothing to say.
		default:
			allAbsent = false
			stuck = append(stuck, fmt.Sprintf("%s (%v)", r.where, r.err))
		}
	}
	if len(stuck) > 0 {
		return fmt.Errorf("the credential may still be readable from %s — revoke it server-side",
			strings.Join(dedupe(stuck), " and "))
	}
	if allAbsent {
		return ErrNoCredential
	}
	return nil
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0:0]
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// --- 0600 file fallback -----------------------------------------------------

type credentialFile struct {
	Credentials map[string]string `yaml:"credentials"`
	// SupersedesKeyring maps a storage key to the SHA-256 of the keyring token
	// that the file entry supersedes.
	//
	// `Get` reads the keyring first, so without something like this a credential
	// rotated on a machine with no keyring is silently overridden by the older
	// desktop entry as soon as the desktop keyring works again.
	//
	// It records the digest of the superseded token, not a bare "the file wins"
	// flag, because the file is the WEAKER store and a flag there would let it
	// grant itself precedence over the stronger one. `credentials.yaml` is
	// exactly the sort of thing restored from a backup or dragged in by a
	// file-sync tool, and a bare flag would turn that into a confused deputy:
	// the operator's commands running under a credential someone else chose. A
	// digest cannot be forged without already knowing the token currently in the
	// keyring, so a planted file can at most reinstate a credential its author
	// already had.
	SupersedesKeyring supersedeMap `yaml:"supersedes-keyring,omitempty"`
}

// supersedeMap tolerates anything it cannot understand.
//
// The field is a HINT. A malformed value used to fail the whole parse, so
// `supersedes-keyring: not-a-list` made every credential unreadable for every
// context rather than degrading one. Unparseable content is dropped, which
// costs at most one keyring-versus-file precedence decision.
type supersedeMap map[string]string

func (m *supersedeMap) UnmarshalYAML(value *yaml.Node) error {
	var raw map[string]string
	if err := value.Decode(&raw); err != nil {
		*m = supersedeMap{}
		return nil
	}
	*m = raw
	return nil
}

// lookup returns the file's token for a key, and whether it came from the
// legacy bare-context-name entry.
func (cf credentialFile) lookup(k Key) (token string, fromLegacy bool) {
	if t := cf.Credentials[k.storage()]; t != "" {
		return t, false
	}
	if t := cf.Credentials[k.legacy()]; t != "" {
		return t, true
	}
	return "", false
}

type precedence int

const (
	neither precedence = iota
	preferKeyring
	preferFile
	ambiguous
)

// precedence decides which store wins.
//
// The keyring is the stronger store and wins by default. The file can only
// outrank it by presenting EVIDENCE — the digest of the keyring value it
// superseded — which it cannot produce without already knowing that value. That
// is what stops a `credentials.yaml` restored from a backup or dragged in by a
// file-sync tool from promoting itself over a live keyring entry and running the
// operator's commands under a credential someone else chose.
//
// The one case with no evidence available is a rotation performed while the
// keyring was unreadable: the writer genuinely could not record what it
// superseded. That is marked `unknown`, and an unknown marker facing a
// DIFFERENT live keyring token is reported as ambiguous rather than resolved in
// either direction.
func (cf credentialFile) precedence(k Key, keyringToken, fileToken string) precedence {
	switch {
	case keyringToken == "" && fileToken == "":
		return neither
	case fileToken == "":
		return preferKeyring
	case keyringToken == "":
		return preferFile
	case keyringToken == fileToken:
		return preferKeyring
	}

	marker, ok := cf.SupersedesKeyring[k.storage()]
	if !ok {
		marker, ok = cf.SupersedesKeyring[k.legacy()]
	}
	switch {
	case !ok || marker == "":
		// No claim at all: the keyring is authoritative.
		return preferKeyring
	case marker == supersedeUnknown:
		return ambiguous
	case subtle.ConstantTimeCompare([]byte(marker), []byte(hashToken(keyringToken))) == 1:
		// Verified: the file names exactly the token the keyring still holds.
		return preferFile
	default:
		// The marker names something the keyring no longer holds, so the keyring
		// has been written since and has taken authority back.
		return preferKeyring
	}
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// loadFile reads the credential file, yielding an empty one when it is absent.
func (s *Store) loadFile() (credentialFile, error) {
	cf := credentialFile{Credentials: map[string]string{}, SupersedesKeyring: supersedeMap{}}
	if s.FilePath == "" {
		return cf, nil
	}
	raw, err := os.ReadFile(s.FilePath)
	if errors.Is(err, os.ErrNotExist) {
		return cf, nil
	}
	if err != nil {
		return cf, fmt.Errorf("read credential file: %w", err)
	}
	if err := yaml.Unmarshal(raw, &cf); err != nil {
		return cf, fmt.Errorf("parse credential file: %w", err)
	}
	if cf.Credentials == nil {
		cf.Credentials = map[string]string{}
	}
	if cf.SupersedesKeyring == nil {
		cf.SupersedesKeyring = supersedeMap{}
	}
	return cf, nil
}

func (s *Store) writeFileLocked(k Key, token, supersedeHash string) error {
	if s.FilePath == "" {
		return errors.New("no credential file configured")
	}
	cf, err := s.loadFile()
	if err != nil {
		return err
	}
	delete(cf.Credentials, k.legacy())
	delete(cf.SupersedesKeyring, k.legacy())
	cf.Credentials[k.storage()] = token
	if supersedeHash == "" {
		delete(cf.SupersedesKeyring, k.storage())
	} else {
		cf.SupersedesKeyring[k.storage()] = supersedeHash
	}
	return s.saveFile(cf)
}

func (s *Store) deleteFileLocked(k Key) error {
	if s.FilePath == "" {
		return ErrNoCredential
	}
	if _, err := os.Stat(s.FilePath); errors.Is(err, os.ErrNotExist) {
		return ErrNoCredential
	}
	cf, err := s.loadFile()
	if err != nil {
		return err
	}
	_, hasNew := cf.Credentials[k.storage()]
	_, hasOld := cf.Credentials[k.legacy()]
	_, markNew := cf.SupersedesKeyring[k.storage()]
	_, markOld := cf.SupersedesKeyring[k.legacy()]
	if !hasNew && !hasOld && !markNew && !markOld {
		return ErrNoCredential
	}
	delete(cf.Credentials, k.storage())
	delete(cf.Credentials, k.legacy())
	delete(cf.SupersedesKeyring, k.storage())
	delete(cf.SupersedesKeyring, k.legacy())
	// Through the same atomic, private path as a write: removing one entry
	// rewrites the whole file, so a direct WriteFile here risks truncating it
	// and losing the OTHER contexts' credentials on a crash.
	return s.saveFile(cf)
}

// saveFile writes the credential file atomically and privately.
//
// `os.CreateTemp` rather than a fixed `<path>.tmp`, for three separate reasons,
// all of which were live bugs in the fixed-name version:
//
//   - `os.WriteFile`'s mode argument applies only when it CREATES the file. A
//     pre-existing `credentials.yaml.tmp` — left by a crash, or planted, since
//     the name was entirely predictable — kept its own permissions, so the
//     credential could land in a 0666 file and the rename carried that mode onto
//     the real one.
//   - if that predictable path was a SYMLINK, the write followed it and the
//     token was written wherever it pointed.
//   - `CreateTemp` uses O_CREATE|O_EXCL with mode 0600, so it cannot open an
//     existing file, cannot follow a symlink, and is private from the instant it
//     exists.
//
// Sync before rename, so a crash leaves either the old file or the new one and
// never a truncated one. Chmod after rename because the destination may already
// exist with looser permissions that the rename preserves.
func (s *Store) saveFile(cf credentialFile) error {
	dir := filepath.Dir(s.FilePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create credential directory: %w", err)
	}
	// MkdirAll does not tighten a directory that already exists, and a
	// group-writable one lets someone else replace the file wholesale.
	if info, err := os.Stat(dir); err == nil {
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			if err := os.Chmod(dir, 0o700); err != nil {
				return fmt.Errorf("credential directory %s is mode %o and could not be tightened: %w",
					dir, perm, err)
			}
		}
	}

	out, err := yaml.Marshal(cf)
	if err != nil {
		return fmt.Errorf("serialize credential file: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".credentials-*")
	if err != nil {
		return fmt.Errorf("write credential file: %w", err)
	}
	tmpName := tmp.Name()
	// Every failure past this point removes the temp file, which holds the
	// token in the clear.
	cleanup := func(cause error, verb string) error {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("%s credential file: %w", verb, cause)
	}
	if _, err := tmp.Write(out); err != nil {
		return cleanup(err, "write")
	}
	if err := tmp.Sync(); err != nil {
		return cleanup(err, "flush")
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("write credential file: %w", err)
	}
	if err := os.Rename(tmpName, s.FilePath); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("write credential file: %w", err)
	}
	if err := os.Chmod(s.FilePath, 0o600); err != nil {
		return fmt.Errorf("restrict credential file permissions: %w", err)
	}
	return nil
}

// --- serialisation ----------------------------------------------------------

// Each write is atomic on its own, but the file holds EVERY context, so a write
// is a read-modify-write over the whole thing. Unserialised, two shells running
// `auth login` for different contexts silently dropped one of them — measured at
// 6 survivors out of 24 concurrent writers.
//
// Two layers, because there are two ways to race:
//
//   - an in-process mutex per file path, which is what makes concurrent use
//     within one binary correct (and `-race` clean);
//   - an exclusive lock FILE, which is the only thing that serialises two
//     separate `drift` processes.
var procLocks sync.Map // path -> *sync.Mutex

// Variables rather than constants so the lock tests can run against short
// values instead of spending half a minute proving a timeout fires. The
// production defaults are pinned by their own test.
var (
	lockPoll    = 20 * time.Millisecond
	lockTimeout = 5 * time.Second
	// A lock older than this is assumed to belong to a process that died before
	// releasing it. Generous relative to the work it guards, which is a few
	// milliseconds of read-modify-write.
	lockStale = 30 * time.Second
)

// withLock serialises a read-modify-write of the credential file.
func (s *Store) withLock(fn func() error) error {
	if s.FilePath == "" {
		return fn()
	}
	mu, _ := procLocks.LoadOrStore(s.FilePath, &sync.Mutex{})
	m := mu.(*sync.Mutex)
	m.Lock()
	defer m.Unlock()

	lock, err := acquireFileLock(s.FilePath + ".lock")
	if err != nil {
		return err
	}
	defer lock.release()
	return fn()
}

// fileWorkNeeded reports whether the credential file exists at all.
//
// The lock guards THAT FILE and nothing else, so a keyring-only operation must
// not be gated on being able to create a lock beside it. A read-only config
// directory would otherwise take down the store that works because of the store
// that does not.
func (s *Store) fileWorkNeeded() bool {
	if s.FilePath == "" {
		return false
	}
	_, err := os.Lstat(s.FilePath)
	return err == nil
}

// fileLock is a held lock plus the token proving it is ours.
type fileLock struct {
	path  string
	nonce string
}

// release removes the lock file, but only while it still holds OUR nonce.
//
// An unconditional remove lets a departing holder delete a lock that someone
// else has since acquired — which is how a "released" lock ends up protecting
// nothing.
func (l fileLock) release() {
	if l.path == "" {
		return
	}
	if current, err := os.ReadFile(l.path); err != nil || string(current) != l.nonce {
		return
	}
	_ = os.Remove(l.path)
}

// acquireFileLock takes an exclusive lock by creating a file with O_EXCL.
//
// O_EXCL rather than flock: no build tags, and the same behaviour on every
// platform the CLI ships to. The caveat worth stating plainly is NFS —
// `O_CREAT|O_EXCL` is not atomic on NFSv2 and is only reliable against an
// NFSv3-or-later server that implements exclusive create correctly; the portable
// idiom there is create-then-link. This is not that, so on an NFS-mounted home
// directory the lock degrades to advisory. The consequence is bounded: the
// in-process mutex still holds, and the file it guards is written atomically by
// rename, so the worst case is the lost-update it was added to prevent rather
// than a corrupt file.
//
// A crash leaves the file behind, which the staleness break handles.
func acquireFileLock(path string) (fileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fileLock{}, fmt.Errorf("create credential directory: %w", err)
	}
	nonce, err := lockNonce()
	if err != nil {
		return fileLock{}, err
	}

	deadline := time.Now().Add(lockTimeout)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, werr := f.WriteString(nonce)
			cerr := f.Close()
			if werr != nil || cerr != nil {
				// A lock we cannot identify later is worse than none: release
				// would either skip a lock that is ours or remove one that is
				// not.
				_ = os.Remove(path)
				return fileLock{}, fmt.Errorf("lock credential file: %w", errors.Join(werr, cerr))
			}
			return fileLock{path: path, nonce: nonce}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return fileLock{}, lockError(path, err)
		}

		// Staleness break. Every path below falls through to the deadline check
		// and the sleep: an earlier version `continue`d here, so a lock that was
		// stale AND could not be removed — a directory at the lock path, or a
		// config directory left root-owned by one `sudo drift auth login` —
		// span at 100% CPU forever, with every credential write hanging behind
		// it and no timeout to escape through.
		breakStaleLock(path)

		if time.Now().After(deadline) {
			return fileLock{}, fmt.Errorf(
				"timed out after %s waiting for the credential lock at %s; "+
					"if no other drift process is running, remove that file",
				lockTimeout, path)
		}
		time.Sleep(lockPoll)
	}
}

// breakStaleLock removes a lock left behind by a process that died.
//
// `Lstat`, not `Stat`: a dangling symlink at the lock path makes `Stat` fail, so
// a `Stat`-based check never fires and every write blocks for the full timeout
// forever. A symlink is never something this code created, so it is removed on
// sight rather than aged.
//
// The removal is guarded by re-reading the file immediately beforehand and
// requiring the contents to be unchanged. That is still check-then-act — a
// holder could acquire between the read and the unlink — but it narrows the
// window to two syscalls and, more importantly, means a lock that has been
// TAKEN OVER since we judged it stale is no longer removed.
func breakStaleLock(path string) {
	info, err := os.Lstat(path)
	if err != nil {
		return
	}
	if info.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(path)
		return
	}
	if !info.Mode().IsRegular() {
		// A directory or a device at the lock path is not a lock and will never
		// age out of being one. Try once; if it cannot be removed, the loop
		// times out with a message naming the path.
		_ = os.Remove(path)
		return
	}
	if time.Since(info.ModTime()) <= lockStale {
		return
	}
	before, err := os.ReadFile(path)
	if err != nil {
		return
	}
	after, err := os.Lstat(path)
	if err != nil || after.ModTime() != info.ModTime() {
		return
	}
	if current, err := os.ReadFile(path); err != nil || string(current) != string(before) {
		return
	}
	_ = os.Remove(path)
}

// lockError names the likely cause rather than the symptom. A permission error
// on the lock file is almost always a config directory the user cannot write,
// and saying "lock credential file: permission denied" sends people looking at
// the wrong thing.
func lockError(path string, err error) error {
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("cannot write to the credential directory %s: %w",
			filepath.Dir(path), err)
	}
	return fmt.Errorf("lock credential file: %w", err)
}

func lockNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate lock token: %w", err)
	}
	return fmt.Sprintf("%d:%s", os.Getpid(), hex.EncodeToString(b[:])), nil
}
