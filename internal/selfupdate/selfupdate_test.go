package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestIsReleaseBuild(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{"1.2.3", true},
		{"0.1.0-dev", false},
		{"1.2.3-rc1", false},
		{"", false},
		{"v1.2.3", false},
		{"0.15.1-next", false},
		{"1.2", false},
		{"1.2.3.4", false},
	}
	for _, c := range cases {
		if got := IsReleaseBuild(c.version); got != c.want {
			t.Errorf("IsReleaseBuild(%q) = %v, want %v", c.version, got, c.want)
		}
	}
}

func TestAssetName(t *testing.T) {
	cases := []struct {
		version, goos, goarch, want string
	}{
		{"0.15.0", "linux", "amd64", "drift-cli_0.15.0_linux_amd64.tar.gz"},
		{"0.15.0", "darwin", "arm64", "drift-cli_0.15.0_darwin_arm64.tar.gz"},
		{"0.15.0", "windows", "amd64", "drift-cli_0.15.0_windows_amd64.zip"},
	}
	for _, c := range cases {
		if got := AssetName(c.version, c.goos, c.goarch); got != c.want {
			t.Errorf("AssetName(%q, %q, %q) = %q, want %q", c.version, c.goos, c.goarch, got, c.want)
		}
	}
	if got := ChecksumsAssetName(); got != "checksums.txt" {
		t.Errorf("ChecksumsAssetName() = %q, want checksums.txt", got)
	}
}

func TestIsMiseManaged(t *testing.T) {
	t.Setenv("MISE_DATA_DIR", "/custom/mise")
	home, _ := os.UserHomeDir()

	managed := []string{
		filepath.Join(home, ".local", "share", "mise", "installs", "drift", "0.15.0", "bin", "drift"),
		"/custom/mise/installs/drift/0.14.0/bin/drift",
	}
	for _, p := range managed {
		if !isMiseManaged(p) {
			t.Errorf("isMiseManaged(%q) = false, want true", p)
		}
	}
	unmanaged := []string{
		"/usr/local/bin/drift",
		"/opt/bin/drift",
		filepath.Join(home, ".local", "bin", "drift"),
	}
	for _, p := range unmanaged {
		if isMiseManaged(p) {
			t.Errorf("isMiseManaged(%q) = true, want false", p)
		}
	}
}

func TestIsBrewManaged(t *testing.T) {
	t.Setenv("HOMEBREW_PREFIX", "/custom/brew")

	managed := []string{
		"/opt/homebrew/bin/drift",
		"/usr/local/Cellar/drift-cli/0.15.0/bin/drift",
		"/custom/brew/bin/drift",
	}
	for _, p := range managed {
		if !isBrewManaged(p) {
			t.Errorf("isBrewManaged(%q) = false, want true", p)
		}
	}
	unmanaged := []string{
		"/usr/local/bin/drift",
		"/opt/bin/drift",
	}
	for _, p := range unmanaged {
		if isBrewManaged(p) {
			t.Errorf("isBrewManaged(%q) = true, want false", p)
		}
	}
}

func TestCheckManagedInstall(t *testing.T) {
	home, _ := os.UserHomeDir()
	mise := filepath.Join(home, ".local", "share", "mise", "installs", "drift", "bin", "drift")
	if err := checkManagedInstall(mise); err == nil || !strings.Contains(err.Error(), "mise") {
		t.Errorf("mise path: got %v, want a mise-naming error", err)
	}
	if err := checkManagedInstall("/opt/homebrew/bin/drift"); err == nil || !strings.Contains(err.Error(), "Homebrew") {
		t.Errorf("brew path: got %v, want a Homebrew-naming error", err)
	}
	if err := checkManagedInstall("/usr/local/bin/drift"); err != nil {
		t.Errorf("unmanaged path: got %v, want nil", err)
	}
}

func TestIsNewer(t *testing.T) {
	cases := []struct {
		candidate, current string
		want               bool
	}{
		{"0.15.0", "0.14.2", true},
		{"0.14.2", "0.15.0", false},
		{"0.15.0", "0.15.0", false},
		{"1.0.0", "0.99.99", true},
	}
	for _, c := range cases {
		if got := IsNewer(c.candidate, c.current); got != c.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", c.candidate, c.current, got, c.want)
		}
	}
}

func TestIsStale(t *testing.T) {
	now := time.Now()
	fresh := &Cache{CheckedAt: now.Add(-1 * time.Hour), Latest: "0.15.0", Current: "0.14.2"}
	old := &Cache{CheckedAt: now.Add(-8 * 24 * time.Hour), Latest: "0.15.0", Current: "0.14.2"}
	versionMismatch := &Cache{CheckedAt: now.Add(-1 * time.Hour), Latest: "0.15.0", Current: "0.13.0"}

	cases := []struct {
		name    string
		cache   *Cache
		current string
		want    bool
	}{
		{"fresh cache not stale", fresh, "0.14.2", false},
		{"old cache is stale", old, "0.14.2", true},
		{"version mismatch is stale", versionMismatch, "0.14.2", true},
		{"zero cache is stale", &Cache{}, "0.14.2", true},
		{"nil cache is stale", nil, "0.14.2", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsStale(c.cache, c.current, now); got != c.want {
				t.Errorf("IsStale() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := &Cache{CheckedAt: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC), Latest: "0.15.0", Current: "0.14.2"}
	if err := WriteCache(dir, c); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	got, err := ReadCache(dir)
	if err != nil {
		t.Fatalf("ReadCache: %v", err)
	}
	if !got.CheckedAt.Equal(c.CheckedAt) || got.Latest != c.Latest || got.Current != c.Current {
		t.Errorf("round trip = %+v, want %+v", got, c)
	}

	// Written with 0600 permissions, like the credential file.
	info, err := os.Stat(CachePath(dir))
	if err != nil {
		t.Fatalf("stat cache: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("cache mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestReadCacheMissingAndCorrupt(t *testing.T) {
	dir := t.TempDir()

	// Missing file: zero cache, no error.
	got, err := ReadCache(dir)
	if err != nil {
		t.Fatalf("missing: ReadCache error = %v, want nil", err)
	}
	if !got.CheckedAt.IsZero() || got.Latest != "" || got.Current != "" {
		t.Errorf("missing: got %+v, want zero cache", got)
	}

	// Corrupt file: zero cache, no error.
	if err := os.WriteFile(CachePath(dir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = ReadCache(dir)
	if err != nil {
		t.Fatalf("corrupt: ReadCache error = %v, want nil", err)
	}
	if !got.CheckedAt.IsZero() || got.Latest != "" || got.Current != "" {
		t.Errorf("corrupt: got %+v, want zero cache", got)
	}
}

// makeTarGz builds a goreleaser-style tar.gz containing a `drift` binary.
func makeTarGz(t *testing.T, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: "drift", Mode: 0o755, Size: int64(len(content))}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// newReleaseServer serves a release with one archive asset plus checksums.txt.
func newReleaseServer(t *testing.T, archive []byte, assetName string) (url string, release *Release) {
	t.Helper()
	digest := sha256.Sum256(archive)
	checksum := hex.EncodeToString(digest[:])
	mux := http.NewServeMux()
	mux.HandleFunc("/"+assetName, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	})
	mux.HandleFunc("/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", checksum, assetName)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	release = &Release{
		Version: "0.15.0",
		Assets: []Asset{
			{Name: assetName, DownloadURL: srv.URL + "/" + assetName},
			{Name: "checksums.txt", DownloadURL: srv.URL + "/checksums.txt"},
		},
	}
	return srv.URL, release
}

// serveReleasesAPI points the package at a fake GitHub releases API for the
// duration of the test.
func serveReleasesAPI(t *testing.T, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Errorf("request had no User-Agent header")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	old := releasesAPI
	releasesAPI = srv.URL
	t.Cleanup(func() { releasesAPI = old })
}

func TestCheck(t *testing.T) {
	serveReleasesAPI(t, `{
		"tag_name": "0.15.0",
		"html_url": "https://github.com/steadfast-ly/drift-cli/releases/tag/0.15.0",
		"assets": [{"name": "checksums.txt", "browser_download_url": "https://example/checksums.txt"}]
	}`)

	res, _, err := Check(context.Background(), http.DefaultClient, "0.14.2")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !res.UpdateAvailable || res.CurrentVersion != "0.14.2" || res.LatestVersion != "0.15.0" {
		t.Errorf("unexpected result: %+v", res)
	}
	if res.ReleaseURL != "https://github.com/steadfast-ly/drift-cli/releases/tag/0.15.0" {
		t.Errorf("ReleaseURL = %q", res.ReleaseURL)
	}

	// Same version: no update.
	res, _, err = Check(context.Background(), http.DefaultClient, "0.15.0")
	if err != nil {
		t.Fatalf("Check (current): %v", err)
	}
	if res.UpdateAvailable {
		t.Errorf("expected no update when current == latest, got %+v", res)
	}
}

func TestCheckVPrefixStrip(t *testing.T) {
	// The real GitHub API returns tag_name with a `v` prefix (e.g. v0.15.0);
	// both LatestRelease and Check must strip it so the version matches the
	// embedded client version and the asset filenames.
	serveReleasesAPI(t, `{
		"tag_name": "v0.15.0",
		"html_url": "https://github.com/steadfast-ly/drift-cli/releases/tag/v0.15.0",
		"assets": [{"name": "checksums.txt", "browser_download_url": "https://example/checksums.txt"}]
	}`)

	rel, err := LatestRelease(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatalf("LatestRelease: %v", err)
	}
	if rel.Version != "0.15.0" {
		t.Errorf("LatestRelease version = %q, want %q (v prefix stripped)", rel.Version, "0.15.0")
	}

	res, _, err := Check(context.Background(), http.DefaultClient, "0.14.0")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !res.UpdateAvailable {
		t.Errorf("expected update available for 0.14.0 vs v0.15.0, got %+v", res)
	}
	if res.LatestVersion != "0.15.0" {
		t.Errorf("LatestVersion = %q, want %q", res.LatestVersion, "0.15.0")
	}
}

func TestCheckAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	old := releasesAPI
	releasesAPI = srv.URL
	t.Cleanup(func() { releasesAPI = old })

	if _, _, err := Check(context.Background(), http.DefaultClient, "0.14.2"); err == nil {
		t.Fatal("expected error for non-200 API response, got nil")
	}
}

func TestDownloadAndVerifyHappyPath(t *testing.T) {
	assetName := AssetName("0.15.0", "linux", "amd64")
	if runtime.GOOS == "windows" {
		assetName = AssetName("0.15.0", "windows", "amd64")
	}
	content := []byte("#!/bin/sh\necho new drift\n")
	archive := makeTarGz(t, content)
	_, release := newReleaseServer(t, archive, assetName)

	got, err := downloadAndVerify(context.Background(), http.DefaultClient, release, assetName, ChecksumsAssetName())
	if err != nil {
		t.Fatalf("downloadAndVerify: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("extracted binary = %q, want %q", got, content)
	}
}

func TestDownloadAndVerifyChecksumMismatch(t *testing.T) {
	assetName := AssetName("0.15.0", "linux", "amd64")
	if runtime.GOOS == "windows" {
		assetName = AssetName("0.15.0", "windows", "amd64")
	}
	archive := makeTarGz(t, []byte("content"))

	// Serve the real archive but a deliberately wrong checksum.
	digest := sha256.Sum256([]byte("something else"))
	wrong := hex.EncodeToString(digest[:])
	mux := http.NewServeMux()
	mux.HandleFunc("/"+assetName, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	})
	mux.HandleFunc("/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", wrong, assetName)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	bad := &Release{
		Version: "0.15.0",
		Assets: []Asset{
			{Name: assetName, DownloadURL: srv.URL + "/" + assetName},
			{Name: "checksums.txt", DownloadURL: srv.URL + "/checksums.txt"},
		},
	}

	if _, err := downloadAndVerify(context.Background(), http.DefaultClient, bad, assetName, ChecksumsAssetName()); err == nil {
		t.Fatal("expected checksum mismatch error, got nil")
	}
}

func TestDownloadAndVerifyMissingAsset(t *testing.T) {
	rel := &Release{Version: "0.15.0", Assets: nil}
	if _, err := downloadAndVerify(context.Background(), http.DefaultClient, rel, "nope.tar.gz", ChecksumsAssetName()); err == nil {
		t.Fatal("expected missing-asset error, got nil")
	}
}

func TestReplaceBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows replacement path differs")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "drift")
	orig := []byte("old binary")
	if err := os.WriteFile(exe, orig, 0o755); err != nil {
		t.Fatal(err)
	}

	newBin := []byte("new binary v0.15.0")
	if err := replaceBinary(exe, newBin); err != nil {
		t.Fatalf("replaceBinary: %v", err)
	}

	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, newBin) {
		t.Errorf("binary = %q, want %q", got, newBin)
	}
	info, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755 (inherited from original)", info.Mode().Perm())
	}

	// No stray temp files left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".drift-update-") {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
}
