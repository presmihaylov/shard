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

	"github.com/presmihaylov/shard/pkg/store"
)

// sumsAsset is the checksum list every shard release carries beside its binaries.
const sumsAsset = "SHA256SUMS"

// stableTag is a shard release; SDK and kernel releases share the repository under other tags.
var stableTag = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

// Release is one published shard release and the files under it.
type Release struct {
	Tag    string  `json:"tag_name"`
	Draft  bool    `json:"draft"`
	Pre    bool    `json:"prerelease"`
	Assets []Asset `json:"assets"`
}

// Asset is one file of a release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
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

// ReleaseByTag is the release named tag, the one a running binary came from.
func ReleaseByTag(ctx context.Context, h Host, tag string) (Release, error) {
	var r Release
	if err := getJSON(ctx, h, h.Releases+"/tags/"+url.PathEscape(tag), &r); err != nil {
		return Release{}, fmt.Errorf("find shard release %s: %w", tag, err)
	}

	return r, nil
}

// FetchAsset downloads the file name of release tag to dst, and only once its hash matches the release's SHA256SUMS.
func FetchAsset(ctx context.Context, h Host, tag, name, dst string, perm fs.FileMode) error {
	r, err := ReleaseByTag(ctx, h, tag)
	if err != nil {
		return err
	}

	return r.Fetch(ctx, h, name, dst, perm)
}

// AssetURL is where release tag serves the file name, for a check that probes access without a download.
func AssetURL(ctx context.Context, h Host, tag, name string) (string, error) {
	r, err := ReleaseByTag(ctx, h, tag)
	if err != nil {
		return "", err
	}
	a, err := r.asset(name)
	if err != nil {
		return "", err
	}

	return a.URL, nil
}

// Fetch downloads the file name of r to dst through a sibling part file, renamed into place after the hash matched.
func (r Release) Fetch(ctx context.Context, h Host, name, dst string, perm fs.FileMode) error {
	want, err := r.sum(ctx, h, name)
	if err != nil {
		return err
	}
	a, err := r.asset(name)
	if err != nil {
		return err
	}

	resp, err := get(ctx, h, a.URL, "")
	if err != nil {
		return fmt.Errorf("download %s from shard release %s: %w", name, r.Tag, err)
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
		return errors.Join(fmt.Errorf("download %s from shard release %s: %w", name, r.Tag, err), os.Remove(part))
	}

	if got := hex.EncodeToString(hash.Sum(nil)); got != want {
		return errors.Join(fmt.Errorf("verify %s from shard release %s: sha256 is %s, SHA256SUMS says %s", name, r.Tag, got, want), os.Remove(part))
	}
	if err := os.Rename(part, dst); err != nil {
		return errors.Join(fmt.Errorf("download %s: %w", name, err), os.Remove(part))
	}

	return store.SyncDir(filepath.Dir(dst))
}

// sum is the hash SHA256SUMS of r gives for name.
func (r Release) sum(ctx context.Context, h Host, name string) (string, error) {
	a, err := r.asset(sumsAsset)
	if err != nil {
		return "", err
	}

	resp, err := get(ctx, h, a.URL, "")
	if err != nil {
		return "", fmt.Errorf("download %s of shard release %s: %w", sumsAsset, r.Tag, err)
	}
	defer resp.Body.Close()

	// sha256sum writes "<hash>  <name>", or "<hash> *<name>" in binary mode.
	lines := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	for lines.Scan() {
		sum, file, ok := strings.Cut(lines.Text(), " ")
		if ok && strings.TrimLeft(file, " *") == name {
			return strings.ToLower(sum), nil
		}
	}
	if err := lines.Err(); err != nil {
		return "", fmt.Errorf("read %s of shard release %s: %w", sumsAsset, r.Tag, err)
	}

	return "", fmt.Errorf("%s of shard release %s has no line for %s", sumsAsset, r.Tag, name)
}

func (r Release) asset(name string) (Asset, error) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, nil
		}
	}

	return Asset{}, fmt.Errorf("shard release %s has no file %s", r.Tag, name)
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
	if resp.StatusCode != http.StatusOK {
		return nil, errors.Join(fmt.Errorf("GET %s: %s", u, resp.Status), resp.Body.Close())
	}

	return resp, nil
}
