package selfupdate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// cacheFileName is the on-disk record of the last update check, stored in the
// config directory.
const cacheFileName = "update-check.json"

// CachePath returns the path of the update-check cache within configDir.
func CachePath(configDir string) string {
	return filepath.Join(configDir, cacheFileName)
}

// ReadCache reads the update-check cache. A missing or corrupt file yields an
// empty cache and no error: both are treated as stale by IsStale, so a fresh
// check runs and silently replaces the bad record (D8).
func ReadCache(configDir string) (*Cache, error) {
	c := &Cache{}
	raw, err := os.ReadFile(CachePath(configDir))
	if err != nil {
		return c, nil
	}
	if err := json.Unmarshal(raw, c); err != nil {
		return &Cache{}, nil
	}
	return c, nil
}

// WriteCache marshals and writes the cache to disk with 0600 permissions,
// creating the config directory if needed.
func WriteCache(configDir string, c *Cache) error {
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(CachePath(configDir), raw, 0o600)
}

// stalenessWindow is how long a recorded check is trusted before a fresh one
// is warranted.
const stalenessWindow = 7 * 24 * time.Hour

// IsStale reports whether a fresh check is warranted: no cache yet, a check
// older than 7 days, or the recorded current version differing from the running
// one (a manual binary swap that the cache did not see).
func IsStale(c *Cache, currentVersion string, now time.Time) bool {
	if c == nil || c.CheckedAt.IsZero() {
		return true
	}
	if now.Sub(c.CheckedAt) > stalenessWindow {
		return true
	}
	if c.Current != currentVersion {
		return true
	}
	return false
}
