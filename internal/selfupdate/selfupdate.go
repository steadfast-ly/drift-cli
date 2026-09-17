// Package selfupdate implements `drift self-update`: querying the GitHub
// releases API, verifying the goreleaser-produced checksums, extracting the
// platform binary from the release archive and atomically replacing the
// running executable.
//
// The package deliberately does not touch the drift server or any stored
// credential: updates come from the public GitHub release, anonymous, at well
// within the 60 req/hr/IP limit (the passive nudge caches for 7 days and a
// manual self-update is infrequent).
package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// releasesAPI is the anonymous GitHub endpoint for the latest release. A var
// (not a const) so tests can point it at an httptest server.
var releasesAPI = "https://api.github.com/repos/steadfast-ly/drift-cli/releases/latest"

// userAgent identifies the client to GitHub.
const userAgent = "drift-cli/self-update"

// Release is the relevant subset of a GitHub release.
type Release struct {
	// Version is the release version. GitHub's tag_name carries a `v` prefix
	// (e.g. `v0.12.0`); LatestRelease strips it so the value matches the
	// goreleaser-embedded client version (`0.12.0`) and the asset filenames.
	Version string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`
	HTMLURL string  `json:"html_url"`
}

// Asset is one downloadable file attached to a release.
type Asset struct {
	Name        string `json:"name"`
	DownloadURL string `json:"browser_download_url"`
}

// CheckResult is the outcome of comparing the installed version with the
// latest release.
type CheckResult struct {
	UpdateAvailable bool
	CurrentVersion  string
	LatestVersion   string
	ReleaseURL      string
}

// Cache is the on-disk record of the last update check (see cache.go).
type Cache struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest"`
	Current   string    `json:"current"`
}

// releaseVersionRe matches a clean goreleaser release tag. Anything else — a
// leading `v`, a `-dev`/`-rc`/`-next` suffix, a git describe string — is a dev
// or non-goreleaser build and cannot self-update.
var releaseVersionRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// IsReleaseBuild reports whether version is a clean X.Y.Z release.
func IsReleaseBuild(version string) bool {
	return releaseVersionRe.MatchString(version)
}

// LatestRelease fetches the most recent release from the GitHub API. The
// timeout is the caller's responsibility (a context or the client's Timeout).
func LatestRelease(ctx context.Context, httpClient *http.Client) (*Release, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releasesAPI, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %s", resp.Status)
	}
	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("parse release: %w", err)
	}
	// goreleaser tags are `vX.Y.Z`, but the embedded client version and
	// the asset filenames use bare `X.Y.Z`.
	rel.Version = strings.TrimPrefix(rel.Version, "v")
	return &rel, nil
}

// Check compares the installed version against the latest release. It returns
// the fetched Release alongside the CheckResult so callers that go on to apply
// an update reuse the same release instead of re-fetching (which could observe
// a different, newer release than the one just checked).
func Check(ctx context.Context, httpClient *http.Client, currentVersion string) (*CheckResult, *Release, error) {
	rel, err := LatestRelease(ctx, httpClient)
	if err != nil {
		return nil, nil, err
	}
	return &CheckResult{
		UpdateAvailable: IsNewer(rel.Version, currentVersion),
		CurrentVersion:  currentVersion,
		LatestVersion:   rel.Version,
		ReleaseURL:      rel.HTMLURL,
	}, rel, nil
}

// IsNewer reports whether candidate is a strictly newer release than current.
func IsNewer(candidate, current string) bool {
	return compareVersions(candidate, current) > 0
}

// compareVersions compares two X.Y.Z versions numerically.
func compareVersions(a, b string) int {
	av := splitVersion(a)
	bv := splitVersion(b)
	for i := 0; i < 3; i++ {
		if av[i] != bv[i] {
			if av[i] > bv[i] {
				return 1
			}
			return -1
		}
	}
	return 0
}

func splitVersion(v string) [3]int {
	var out [3]int
	for i, part := range strings.SplitN(v, ".", 3) {
		n, _ := strconv.Atoi(part)
		out[i] = n
	}
	return out
}

// AssetName builds the goreleaser archive filename for a platform. goreleaser
// uses Go's own GOOS/GOARCH naming, so runtime.GOOS/runtime.GOARCH map directly.
func AssetName(version, goos, goarch string) string {
	ext := "tar.gz"
	if goos == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("drift-cli_%s_%s_%s.%s", version, goos, goarch, ext)
}

// ChecksumsAssetName is the checksum file goreleaser attaches to every release.
func ChecksumsAssetName() string { return "checksums.txt" }

// Apply downloads the platform archive for the release, verifies its SHA256
// against checksums.txt, extracts the binary and atomically replaces the
// running executable.
func Apply(ctx context.Context, httpClient *http.Client, release *Release, currentVersion string) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}
	// A mise or brew install is often a symlink into a versioned install dir;
	// resolve to the real path so the managed-install heuristic sees it.
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	if err := checkManagedInstall(exePath); err != nil {
		return err
	}

	assetName := AssetName(release.Version, runtime.GOOS, runtime.GOARCH)
	binary, err := downloadAndVerify(ctx, httpClient, release, assetName, ChecksumsAssetName())
	if err != nil {
		return err
	}
	return replaceBinary(exePath, binary)
}

// checkManagedInstall returns a descriptive error when the binary lives under
// a mise or Homebrew install directory. Those package managers own the binary
// and have their own upgrade paths; silently replacing the file under them
// would be clobbered or shadowed on the next `mise upgrade`/`brew upgrade`.
func checkManagedInstall(exePath string) error {
	if isMiseManaged(exePath) {
		return fmt.Errorf("%s is managed by mise; use `mise upgrade drift` (or `mise install drift`) instead of self-update", exePath)
	}
	if isBrewManaged(exePath) {
		return fmt.Errorf("%s is managed by Homebrew; use `brew upgrade drift-cli` instead of self-update", exePath)
	}
	return nil
}

// isMiseManaged reports whether the binary lives under a mise install dir.
func isMiseManaged(exePath string) bool {
	for _, prefix := range misePrefixes() {
		if strings.HasPrefix(exePath, prefix) {
			return true
		}
	}
	return false
}

func misePrefixes() []string {
	var dirs []string
	if v := os.Getenv("MISE_DATA_DIR"); v != "" {
		dirs = append(dirs, filepath.Join(v, "installs"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".local", "share", "mise", "installs"))
	}
	return dirs
}

// isBrewManaged reports whether the binary lives under a Homebrew install.
func isBrewManaged(exePath string) bool {
	for _, prefix := range brewPrefixes() {
		if strings.HasPrefix(exePath, prefix) {
			return true
		}
	}
	return false
}

func brewPrefixes() []string {
	var dirs []string
	if v := os.Getenv("HOMEBREW_PREFIX"); v != "" {
		dirs = append(dirs, v)
	}
	dirs = append(dirs, "/opt/homebrew", "/usr/local/Cellar")
	return dirs
}

// downloadAndVerify downloads the release archive and its checksum file,
// verifies the archive's SHA256, and extracts the drift binary.
func downloadAndVerify(ctx context.Context, httpClient *http.Client, release *Release, assetName, checksumsName string) ([]byte, error) {
	assetURL, err := assetDownloadURL(release, assetName)
	if err != nil {
		return nil, err
	}
	checksumsURL, err := assetDownloadURL(release, checksumsName)
	if err != nil {
		return nil, err
	}

	archive, err := download(ctx, httpClient, assetURL)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", assetName, err)
	}
	checksums, err := download(ctx, httpClient, checksumsURL)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", checksumsName, err)
	}

	want, err := checksumFor(checksums, assetName)
	if err != nil {
		return nil, err
	}
	got := sha256.Sum256(archive)
	if !strings.EqualFold(hex.EncodeToString(got[:]), strings.TrimSpace(want)) {
		return nil, fmt.Errorf("checksum mismatch for %s: got %s, want %s",
			assetName, hex.EncodeToString(got[:]), want)
	}

	if strings.HasSuffix(assetName, ".zip") {
		return extractZipBinary(archive)
	}
	return extractTarGzBinary(archive)
}

// assetDownloadURL finds the download URL for the named asset on a release.
func assetDownloadURL(release *Release, name string) (string, error) {
	for _, a := range release.Assets {
		if a.Name == name {
			if a.DownloadURL == "" {
				return "", fmt.Errorf("release %s has no download URL for %s", release.Version, name)
			}
			return a.DownloadURL, nil
		}
	}
	return "", fmt.Errorf("release %s has no asset named %s", release.Version, name)
}

// download fetches a URL body. The client follows redirects (required:
// GitHub's browser_download_url 302s to objects.githubusercontent.com).
// Checksum verification in the caller protects against CDN corruption.
func download(ctx context.Context, httpClient *http.Client, url string) ([]byte, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned %s", resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// checksumFor parses a goreleaser checksums.txt — one `<sha256>  <filename>`
// per line — and returns the digest for the named asset.
func checksumFor(checksums []byte, assetName string) (string, error) {
	for _, line := range strings.Split(string(checksums), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[1] != assetName {
			continue
		}
		return fields[0], nil
	}
	return "", fmt.Errorf("no checksum found for %s", assetName)
}

// extractTarGzBinary reads the `drift` binary out of a goreleaser tar.gz.
func extractTarGzBinary(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("open gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || filepath.Base(hdr.Name) != "drift" {
			continue
		}
		return io.ReadAll(tr)
	}
	return nil, fmt.Errorf("no drift binary found in archive")
}

// extractZipBinary reads the `drift` (or `drift.exe`) binary out of a
// goreleaser zip. Windows archives contain `drift.exe`.
func extractZipBinary(archive []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	for _, f := range zr.File {
		name := filepath.Base(f.Name)
		if f.FileInfo().IsDir() || (name != "drift" && name != "drift.exe") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(rc)
	}
	return nil, fmt.Errorf("no drift binary found in archive")
}

// replaceBinary atomically swaps in the new binary. The temp file is written
// in the target's own directory so the rename stays on one filesystem, and it
// inherits the original binary's mode. On Windows the running executable cannot
// be overwritten, so the old binary is renamed aside first and removed after.
func replaceBinary(exePath string, newBinary []byte) error {
	info, err := os.Stat(exePath)
	if err != nil {
		return fmt.Errorf("stat current binary: %w", err)
	}

	if runtime.GOOS == "windows" {
		return replaceBinaryWindows(exePath, newBinary, info.Mode())
	}

	dir := filepath.Dir(exePath)
	tmp, err := os.CreateTemp(dir, ".drift-update-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(newBinary); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Chmod(info.Mode()); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, exePath); err != nil {
		return fmt.Errorf("replace binary: %w", err)
	}
	return nil
}

func replaceBinaryWindows(exePath string, newBinary []byte, mode os.FileMode) error {
	dir := filepath.Dir(exePath)
	tmp, err := os.CreateTemp(dir, ".drift-update-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(newBinary); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	// The running .exe cannot be overwritten or deleted on Windows. Rename the
	// old binary aside, install the new one, then best-effort remove the .old.
	oldPath := exePath + ".old"
	_ = os.Remove(oldPath)
	if err := os.Rename(exePath, oldPath); err != nil {
		return fmt.Errorf("move current binary aside: %w", err)
	}
	if err := os.Rename(tmpName, exePath); err != nil {
		// Rollback: restore the original binary so the CLI is not bricked.
		if rbErr := os.Rename(oldPath, exePath); rbErr != nil {
			return fmt.Errorf("install new binary: %w (rollback also failed: %v)", err, rbErr)
		}
		return fmt.Errorf("install new binary (rolled back): %w", err)
	}
	_ = os.Remove(oldPath)
	return nil
}
