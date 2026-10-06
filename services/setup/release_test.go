package setup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// releaseServer is a fake releases API, with the download URLs that serve its files.
type releaseServer struct {
	*httptest.Server
	releases  []Release
	files     map[string]string
	api       atomic.Int32
	downloads atomic.Int32
}

func newReleaseServer(t *testing.T) *releaseServer {
	t.Helper()

	rs := &releaseServer{files: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /releases", func(w http.ResponseWriter, r *http.Request) {
		rs.api.Add(1)
		if r.URL.Query().Get("per_page") != "100" {
			http.Error(w, "want per_page=100", http.StatusBadRequest)
			return
		}
		writeJSON(t, w, rs.releases)
	})
	mux.HandleFunc("GET /download/{tag}/{name}", func(w http.ResponseWriter, r *http.Request) {
		rs.downloads.Add(1)
		body, ok := rs.files[r.PathValue("tag")+"/"+r.PathValue("name")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("write %s: %v", r.URL.Path, err)
		}
	})
	rs.Server = httptest.NewServer(mux)
	t.Cleanup(rs.Close)

	return rs
}

// add publishes a release with files, and a SHA256SUMS for them unless sums is false.
func (rs *releaseServer) add(tag string, draft, pre, sums bool, files map[string]string) {
	rel := Release{Tag: tag, Draft: draft, Pre: pre}
	var list strings.Builder
	for name, body := range files {
		rs.files[tag+"/"+name] = body
		sum := sha256.Sum256([]byte(body))
		list.WriteString(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	}
	if sums {
		rs.files[tag+"/"+sumsAsset] = list.String()
	}
	rs.releases = append(rs.releases, rel)
}

func (rs *releaseServer) host() Host {
	return Host{Releases: rs.URL + "/releases", Downloads: rs.URL + "/download", HTTP: rs.Client()}
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode: %v", err)
	}
}

func TestLatestReleasePicksTheHighestStableTag(t *testing.T) {
	rs := newReleaseServer(t)
	// API order is newest first; the pick must not trust it, nor compare tags as text.
	rs.add("sdk-typescript-v0.3.0", false, false, true, nil)
	rs.add("v0.9.1", false, false, true, nil)
	rs.add("kernel-6.12.110-3", false, false, true, nil)
	rs.add("v0.11.0-rc.1", false, false, true, nil)
	rs.add("v0.12.0", false, true, true, nil)
	rs.add("v0.13.0", true, false, true, nil)
	rs.add("v0.10.0", false, false, true, nil)
	rs.add("sdk-python-v0.2.0", false, false, true, nil)

	got, err := LatestRelease(context.Background(), rs.host())
	if err != nil {
		t.Fatalf("LatestRelease: %v", err)
	}
	if got.Tag != "v0.10.0" {
		t.Fatalf("LatestRelease picked %s, want v0.10.0", got.Tag)
	}
}

func TestLatestReleaseRefusesAListWithNoStableTag(t *testing.T) {
	rs := newReleaseServer(t)
	rs.add("sdk-typescript-v0.3.0", false, false, true, nil)
	rs.add("v1.0.0", false, true, true, nil)

	_, err := LatestRelease(context.Background(), rs.host())
	if err == nil || !strings.Contains(err.Error(), "no published v<major>.<minor>.<patch> release") {
		t.Fatalf("LatestRelease error = %v, want the missing stable release named", err)
	}
}

func TestLatestReleaseNamesAnAPIFailure(t *testing.T) {
	reset := time.Unix(1759745745, 0)
	cases := map[string]struct {
		status  int
		headers map[string]string
		want    string
	}{
		"a refusal":               {status: http.StatusForbidden, headers: map[string]string{"X-RateLimit-Remaining": "12"}, want: ": 403 Forbidden"},
		"a spent limit":           {status: http.StatusForbidden, headers: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1759745745"}, want: "the GitHub API rate limit for this IP address is spent (403 Forbidden); it resets at " + reset.Local().Format("15:04 MST") + ", so run shard setup again after that"},
		"a spent limit, no reset": {status: http.StatusTooManyRequests, headers: map[string]string{"X-RateLimit-Remaining": "0"}, want: "is spent (429 Too Many Requests); run shard setup again within an hour"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				http.Error(w, "rate limited", tc.status)
			}))
			t.Cleanup(api.Close)

			_, err := LatestRelease(context.Background(), Host{Releases: api.URL + "/releases", HTTP: api.Client()})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LatestRelease error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestFetchAssetWritesOnlyAVerifiedFile(t *testing.T) {
	rs := newReleaseServer(t)
	rs.add("v0.2.0", false, false, true, map[string]string{"shard-linux-amd64": "new binary"})
	dst := filepath.Join(t.TempDir(), "shard")

	if err := FetchAsset(context.Background(), rs.host(), "v0.2.0", "shard-linux-amd64", dst, 0o755); err != nil {
		t.Fatalf("FetchAsset: %v", err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read %s: %v", dst, err)
	}
	if string(data) != "new binary" {
		t.Fatalf("FetchAsset wrote %q", data)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat %s: %v", dst, err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("FetchAsset mode = %v, want 0755", info.Mode().Perm())
	}
	if rs.api.Load() != 0 {
		t.Fatalf("FetchAsset made %d GitHub API calls, want none", rs.api.Load())
	}
}

func TestFetchAssetRefusesABadOrMissingSum(t *testing.T) {
	cases := map[string]struct {
		asset  string
		sums   bool
		tamper bool
		want   string
	}{
		"no SHA256SUMS":  {asset: "shard-linux-amd64", sums: false, want: "download SHA256SUMS of shard release v0.2.0"},
		"hash mismatch":  {asset: "shard-linux-amd64", sums: true, tamper: true, want: "SHA256SUMS says"},
		"no line for it": {asset: "shard-init-linux-amd64", sums: true, want: "has no line for shard-init-linux-amd64"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rs := newReleaseServer(t)
			rs.add("v0.2.0", false, false, tc.sums, map[string]string{"shard-linux-amd64": "new binary"})
			if tc.tamper {
				rs.files["v0.2.0/shard-linux-amd64"] = "tampered"
			}
			dir := t.TempDir()
			dst := filepath.Join(dir, "shard")

			err := FetchAsset(context.Background(), rs.host(), "v0.2.0", tc.asset, dst, 0o755)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("FetchAsset error = %v, want %q", err, tc.want)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("read %s: %v", dir, err)
			}
			if len(entries) != 0 {
				t.Fatalf("FetchAsset left %v behind", entries)
			}
		})
	}
}

func TestFetchAssetNamesAMissingTag(t *testing.T) {
	rs := newReleaseServer(t)
	rs.add("v0.2.0", false, false, true, nil)

	err := FetchAsset(context.Background(), rs.host(), "dev-abc123", "shard-init-linux-amd64", filepath.Join(t.TempDir(), "x"), 0o755)
	if err == nil || !strings.Contains(err.Error(), "SHA256SUMS of shard release dev-abc123") || !strings.Contains(err.Error(), "404") {
		t.Fatalf("FetchAsset error = %v, want the missing tag and 404 named", err)
	}
}

func TestAssetURLIsTheDownloadURL(t *testing.T) {
	h := Host{Downloads: "https://github.com/presmihaylov/shard/releases/download"}

	if got := AssetURL(h, "v0.2.0", "shard-init-linux-amd64"); got != h.Downloads+"/v0.2.0/shard-init-linux-amd64" {
		t.Fatalf("AssetURL = %q", got)
	}
}
