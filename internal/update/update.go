// Package update checks for newer frostcode releases on GitHub and performs
// an in-place binary replacement when a matching asset is available.
package update

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const repoAPI = "https://api.github.com/repos/Trancezzzz/frostcode/releases/latest"

// Release holds the fields we care about from the GitHub releases API.
type Release struct {
	TagName string  `json:"tag_name"`
	HTMLURL string  `json:"html_url"`
	Assets  []Asset `json:"assets"`
}

// Asset is a single downloadable file attached to a release.
type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// CheckResult is the outcome of a background update check.
type CheckResult struct {
	Release *Release
	Err     error
}

// CheckBackground starts a non-blocking update check and returns a channel that
// receives exactly one CheckResult when the check completes (or times out).
func CheckBackground() <-chan CheckResult {
	ch := make(chan CheckResult, 1)
	go func() {
		rel, err := Check()
		ch <- CheckResult{Release: rel, Err: err}
	}()
	return ch
}

// Check fetches the latest GitHub release and returns it.
// Returns an error if the network request fails or the response is malformed.
func Check() (*Release, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", repoAPI, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "frostcode-updater")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("no releases found at github.com/Trancezzzz/frostcode")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %s", resp.Status)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

// IsNewer reports whether releaseTag is strictly newer than currentVersion
// using semver comparison. Both are expected to be "vMAJOR.MINOR.PATCH".
// Returns false for dev/empty builds so they are treated as up-to-date.
func IsNewer(currentVersion, releaseTag string) bool {
	if currentVersion == "dev" || currentVersion == "" {
		return false
	}
	cur := parseSemver(currentVersion)
	rel := parseSemver(releaseTag)
	for i := range cur {
		if rel[i] > cur[i] {
			return true
		}
		if rel[i] < cur[i] {
			return false
		}
	}
	return false
}

// parseSemver parses a "vMAJOR.MINOR.PATCHsuffix" string into [3]int.
// Non-numeric suffixes on any segment (e.g. the "b" in "v0.1.3b") are stripped
// before parsing so beta tags compare correctly against release tags.
func parseSemver(tag string) [3]int {
	tag = strings.TrimPrefix(tag, "v")
	parts := strings.SplitN(tag, ".", 3)
	var v [3]int
	for i, p := range parts {
		if i >= 3 {
			break
		}
		// strip any trailing non-digit suffix (e.g. "3b" → "3", "0rc1" → "0")
		end := len(p)
		for end > 0 && (p[end-1] < '0' || p[end-1] > '9') {
			end--
		}
		n, _ := strconv.Atoi(p[:end])
		v[i] = n
	}
	return v
}

// BinaryAsset returns the release asset whose name matches the current
// OS and architecture (e.g. "frostcode_linux_amd64", "frostcode_windows_amd64.exe").
// Returns nil if no matching asset exists.
func BinaryAsset(rel *Release) *Asset {
	os_ := runtime.GOOS
	arch := runtime.GOARCH
	// Normalise arch: arm64 → arm64, amd64 → amd64, 386 → 386.
	want := fmt.Sprintf("frostcode_%s_%s", os_, arch)
	if os_ == "windows" {
		want += ".exe"
	}
	for i := range rel.Assets {
		if strings.EqualFold(rel.Assets[i].Name, want) {
			return &rel.Assets[i]
		}
	}
	return nil
}

// ReplaceExe downloads asset and atomically replaces the running binary.
// It writes to a temp file alongside the current exe, then renames it over
// the original — the rename is atomic on all supported platforms.
func ReplaceExe(asset *Asset) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot locate current executable: %w", err)
	}

	client := &http.Client{Timeout: 120 * time.Second}
	req, _ := http.NewRequest("GET", asset.BrowserDownloadURL, nil)
	req.Header.Set("User-Agent", "frostcode-updater")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned %s", resp.Status)
	}

	tmp := exe + ".new"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("cannot write temp file: %w", err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write failed: %w", err)
	}
	f.Close()

	// On Windows we can't rename over a running exe, so we back it up first.
	if runtime.GOOS == "windows" {
		bak := exe + ".old"
		os.Remove(bak) // ignore error; leftover from a previous update
		if err := os.Rename(exe, bak); err != nil {
			os.Remove(tmp)
			return fmt.Errorf("cannot back up current binary: %w", err)
		}
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("cannot replace binary: %w", err)
	}
	return nil
}
