package setup

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/presmihaylov/shard/pkg/store"
)

// sumsAsset is the checksum list every shard release carries beside its binaries.
const sumsAsset = "SHA256SUMS"

// stableTag is a shard release; SDK and kernel releases share the repository under other tags.
var stableTag = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

// Release is one published shard release.
type Release struct {
	Tag   string `json:"tag_name"`
	Draft bool   `json:"draft"`
	Pre   bool   `json:"prerelease"`
}

// LatestRelease is the highest stable v<major>.<minor>.<patch> by number, never the Latest flag, so an SDK or kernel release is never picked.
func LatestRelease(ctx context.Context, h Host) (Release, error) {
	var releases []Release
	if err := getJSON(ctx, h, h.Releases+"?per_page=100", &releases); err != nil {
		return Release{}, fmt.Errorf("list shard releases: %w", err)
	}

	var latest Release
	var latestVersion [3]uint64
	for _, r := range releases {
		v, ok := stableVersion(r.Tag)
		if r.Draft || r.Pre || !ok {
			continue
		}
		if latest.Tag != "" && !newer(v, latestVersion) {
			continue
		}
		latest, latestVersion = r, v
	}
	if latest.Tag == "" {
		return Release{}, fmt.Errorf("list shard releases: %s has no published v<major>.<minor>.<patch> release", h.Releases)
	}

	return latest, nil
}

// FetchAsset downloads the file name of release tag to dst through a sibling part file, renamed into place once its hash matches the release's SHA256SUMS.
func FetchAsset(ctx context.Context, h Host, tag, name, dst string, perm fs.FileMode) error {
	want, err := sum(ctx, h, tag, name)
	if err != nil {
		return err
	}

	resp, err := get(ctx, h, AssetURL(h, tag, name), "")
	if err != nil {
		return fmt.Errorf("download %s from shard release %s: %w", name, tag, err)
	}
	defer resp.Body.Close()

	f, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".*.part")
	if err != nil {
		return fmt.Errorf("download %s: %w", name, err)
	}
	part := f.Name()
	hash := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, hash), resp.Body)
	if err == nil {
		err = f.Chmod(perm)
	}
	if err == nil {
		err = f.Sync()
	}
	if err := errors.Join(err, f.Close()); err != nil {
		return errors.Join(fmt.Errorf("download %s from shard release %s: %w", name, tag, err), os.Remove(part))
	}

	if got := hex.EncodeToString(hash.Sum(nil)); got != want {
		return errors.Join(fmt.Errorf("verify %s from shard release %s: sha256 is %s, SHA256SUMS says %s", name, tag, got, want), os.Remove(part))
	}
	if err := os.Rename(part, dst); err != nil {
		return errors.Join(fmt.Errorf("download %s: %w", name, err), os.Remove(part))
	}

	return store.SyncDir(filepath.Dir(dst))
}

// AssetURL is where release tag serves the file name: the download URL, which spends none of the API rate limit.
func AssetURL(h Host, tag, name string) string {
	return h.Downloads + "/" + url.PathEscape(tag) + "/" + url.PathEscape(name)
}

// sum is the hash the SHA256SUMS of release tag gives for name.
func sum(ctx context.Context, h Host, tag, name string) (string, error) {
	resp, err := get(ctx, h, AssetURL(h, tag, sumsAsset), "")
	if err != nil {
		return "", fmt.Errorf("download %s of shard release %s: %w", sumsAsset, tag, err)
	}
	defer resp.Body.Close()

	// sha256sum writes "<hash>  <name>", or "<hash> *<name>" in binary mode.
	lines := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	for lines.Scan() {
		digest, file, ok := strings.Cut(lines.Text(), " ")
		if ok && strings.TrimLeft(file, " *") == name {
			return strings.ToLower(digest), nil
		}
	}
	if err := lines.Err(); err != nil {
		return "", fmt.Errorf("read %s of shard release %s: %w", sumsAsset, tag, err)
	}

	return "", fmt.Errorf("%s of shard release %s has no line for %s", sumsAsset, tag, name)
}

func stableVersion(tag string) ([3]uint64, bool) {
	m := stableTag.FindStringSubmatch(tag)
	if m == nil {
		return [3]uint64{}, false
	}

	var v [3]uint64
	for i := range v {
		n, err := strconv.ParseUint(m[i+1], 10, 64)
		if err != nil {
			return [3]uint64{}, false
		}
		v[i] = n
	}

	return v, true
}

func newer(a, b [3]uint64) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}

	return false
}

func getJSON(ctx context.Context, h Host, u string, v any) error {
	resp, err := get(ctx, h, u, "application/vnd.github+json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("decode %s: %w", u, err)
	}

	return nil
}

// get answers only a 200, so a rate limit or a missing tag names its status and never reads as an empty list.
func get(ctx context.Context, h Host, u, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}

	resp, err := h.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if limit := rateLimited(resp); limit != nil {
		return nil, errors.Join(fmt.Errorf("GET %s: %w", u, limit), resp.Body.Close())
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errors.Join(fmt.Errorf("GET %s: %s", u, resp.Status), resp.Body.Close())
	}

	return resp, nil
}

// rateLimitError is GitHub refusing every API call from this address until its limit resets.
type rateLimitError struct {
	status string
	reset  time.Time
}

func (e *rateLimitError) Error() string {
	if e.reset.IsZero() {
		return fmt.Sprintf("the GitHub API rate limit for this IP address is spent (%s); run shard setup again within an hour", e.status)
	}

	return fmt.Sprintf("the GitHub API rate limit for this IP address is spent (%s); it resets at %s, so run shard setup again after that", e.status, e.reset.Local().Format("15:04 MST"))
}

// rateLimited is the refusal GitHub sends once no API call is left, with the reset it names; a reset that does not parse still names the limit.
func rateLimited(resp *http.Response) *rateLimitError {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("X-RateLimit-Remaining") != "0" {
		return nil
	}
	reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil {
		return &rateLimitError{status: resp.Status}
	}

	return &rateLimitError{status: resp.Status, reset: time.Unix(reset, 0)}
}
