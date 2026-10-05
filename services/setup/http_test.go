package setup

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// A server that stops sending fails the download after the idle time, with the documented text. (SHARD-681)
func TestAStalledDownloadTimesOut(t *testing.T) {
	for _, tt := range []struct {
		name  string
		serve func(w http.ResponseWriter, r *http.Request)
	}{
		{"the headers never come", func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }},
		{"the body stops", func(w http.ResponseWriter, r *http.Request) {
			if _, err := io.WriteString(w, "partial"); err != nil {
				t.Error(err)
			}
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(tt.serve))
			t.Cleanup(server.Close)
			h := newLocalHost(t).host()
			h.HTTP = newHTTPClient(100 * time.Millisecond)
			p := &localPlan{h: h, missing: missing{downloads: []*download{{Title: "gVisor", URL: server.URL + "/gvisor.tar.zstd", SHA256: "00"}}}}

			err := p.download(t.Context())

			if got, want := problemLines(err), []string{"Could not download gVisor: connection timed out."}; !slices.Equal(got, want) {
				t.Errorf("the download failed with %q, want %q", got, want)
			}
		})
	}
}

// A download slower than the idle time in total still completes while each chunk comes within it. (SHARD-681)
func TestASlowDownloadThatMovesCompletes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for range 8 {
			time.Sleep(50 * time.Millisecond)
			if _, err := io.WriteString(w, "chunk"); err != nil {
				t.Error(err)
			}
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(server.Close)

	resp, err := newHTTPClient(200 * time.Millisecond).Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	})
	body, err := io.ReadAll(resp.Body)

	if err != nil || string(body) != strings.Repeat("chunk", 8) {
		t.Errorf("read %q, %v; want every chunk and no error", body, err)
	}
}
