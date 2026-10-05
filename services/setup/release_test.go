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
)

// releaseServer is a fake releases API whose assets point back at itself.
type releaseServer struct {
	*httptest.Server
	releases  []Release
	files     map[string]string
	downloads atomic.Int32
}

func newReleaseServer(t *testing.T) *releaseServer {
	t.Helper()

	rs := &releaseServer{files: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /releases", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("per_page") != "100" {
			http.Error(w, "want per_page=100", http.StatusBadRequest)
			return
		}
		writeJSON(t, w, rs.releases)
	})
	mux.HandleFunc("GET /releases/tags/{tag}", func(w http.ResponseWriter, r *http.Request) {
		for _, rel := range rs.releases {
			if rel.Tag == r.PathValue("tag") {
				writeJSON(t, w, rel)
				return
			}
		}
		http.NotFound(w, r)
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
		rel.Assets = append(rel.Assets, Asset{Name: name, URL: rs.URL + "/download/" + tag + "/" + name})
		sum := sha256.Sum256([]byte(body))
		list.WriteString(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	}
	if sums {
		rs.files[tag+"/"+sumsAsset] = list.String()
		rel.Assets = append(rel.Assets, Asset{Name: sumsAsset, URL: rs.URL + "/download/" + tag + "/" + sumsAsset})
	}
	rs.releases = append(rs.releases, rel)
}

func (rs *releaseServer) host() Host {
	return Host{Releases: rs.URL + "/releases", HTTP: rs.Client()}
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
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate limited", http.StatusForbidden)
	}))
	t.Cleanup(api.Close)

	_, err := LatestRelease(context.Background(), Host{Releases: api.URL + "/releases", HTTP: api.Client()})
	if err == nil || !strings.Contains(err.Error(), "403 Forbidden") {
		t.Fatalf("LatestRelease error = %v, want the 403 named", err)
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
}

func TestFetchAssetRefusesABadOrMissingSum(t *testing.T) {
	cases := map[string]struct {
		asset  string
		sums   bool
		tamper bool
		want   string
	}{
		"no SHA256SUMS":  {asset: "shard-linux-amd64", sums: false, want: "has no file SHA256SUMS"},
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
	if err == nil || !strings.Contains(err.Error(), "find shard release dev-abc123") || !strings.Contains(err.Error(), "404") {
		t.Fatalf("FetchAsset error = %v, want the missing tag and 404 named", err)
	}
}

func TestAssetURLNamesAMissingFile(t *testing.T) {
	rs := newReleaseServer(t)
	rs.add("v0.2.0", false, false, true, map[string]string{"shard-init-linux-amd64": "init"})

	got, err := AssetURL(context.Background(), rs.host(), "v0.2.0", "shard-init-linux-amd64")
	if err != nil || got != rs.URL+"/download/v0.2.0/shard-init-linux-amd64" {
		t.Fatalf("AssetURL = %q, %v", got, err)
	}
	if rs.downloads.Load() != 0 {
		t.Fatalf("AssetURL downloaded %d files", rs.downloads.Load())
	}
	if _, err := AssetURL(context.Background(), rs.host(), "v0.2.0", "shard-init-linux-arm64"); err == nil || !strings.Contains(err.Error(), "shard-init-linux-arm64") {
		t.Fatalf("AssetURL error = %v, want the missing file named", err)
	}
}
