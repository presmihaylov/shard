package setup

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// A server that stops sending fails the download after the idle time, with the documented text. (SHARD-681)
func TestAStalledDownloadTimesOut(t *testing.T) {
	for _, tt := range []struct {
		name  string
		serve func(w http.ResponseWriter, r *http.Request)
		want  string
	}{
		// No headers means no body yet, so ResponseHeaderTimeout fires and reads as a plain timeout.
		{"the headers never come", func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }, "connection timed out"},
		// A body that stalls is the idle body's own timeout, which names the window it waited.
		{"the body stops", func(w http.ResponseWriter, r *http.Request) {
			if _, err := io.WriteString(w, "partial"); err != nil {
				t.Error(err)
			}
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}, "no data for 100ms; setup stops a download that sends nothing for that long"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(tt.serve))
			t.Cleanup(server.Close)
			h := newLocalHost(t).host()
			h.HTTP = newHTTPClient(100 * time.Millisecond)
			p := &localPlan{h: h, missing: missing{downloads: []*download{{Title: "gVisor", URL: server.URL + "/gvisor.tar.zstd", SHA256: "00"}}}}

			err := p.download(t.Context())

			if got, want := problemLines(err), []string{"Could not download gVisor: " + tt.want + "."}; !slices.Equal(got, want) {
				t.Errorf("the download failed with %q, want %q", got, want)
			}
		})
	}
}

// The download reports how far it got once a second, and once at the end whatever the clock says. (SHARD-744)
func TestADownloadReportsProgressOnceASecondAndAtTheEnd(t *testing.T) {
	clock := time.Unix(1000, 0)
	var lines []string
	pr := &progressReader{
		r:      bytes.NewReader(bytes.Repeat([]byte("x"), 4096)),
		label:  "gVisor",
		total:  4096,
		report: func(detail ...string) error { lines = append(lines, detail...); return nil },
		now:    func() time.Time { return clock }, // a fixed clock, so only the first read and the end report
	}

	buf := make([]byte, 1024)
	for {
		n, err := pr.Read(buf)
		if n == 0 && errors.Is(err, io.EOF) {
			break
		}
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("read: %v", err)
		}
	}

	want := []string{"gVisor 1.0 KiB of 4.0 KiB (25%)", "gVisor 4.0 KiB of 4.0 KiB (100%)"}
	if !slices.Equal(lines, want) {
		t.Errorf("reported %q, want %q", lines, want)
	}
}

// progressLine shows a percent with a known length and bytes alone without one. (SHARD-744)
func TestProgressLine(t *testing.T) {
	for _, c := range []struct {
		read, total int64
		want        string
	}{
		{512, 2048, "runsc 512 B of 2.0 KiB (25%)"},
		{1572864, 3145728, "runsc 1.5 MiB of 3.0 MiB (50%)"},
		{100, 0, "runsc 100 B"},
	} {
		if got := progressLine("runsc", c.read, c.total); got != c.want {
			t.Errorf("progressLine(%d, %d) = %q, want %q", c.read, c.total, got, c.want)
		}
	}
}

// A download slower than the idle time in total still completes while each chunk comes within it. (SHARD-681)
func TestASlowDownloadThatMovesCompletes(t *testing.T) {
	const idle = 200 * time.Millisecond
	next := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		for range 8 {
			select {
			case <-next:
			case <-r.Context().Done():
				return
			}
			if _, err := io.WriteString(w, "chunk"); err != nil {
				t.Error(err)
			}
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(server.Close)
	client := newHTTPClient(idle)
	clock := &fakeTimer{}
	client.Transport.(*idleTransport).after = clock.start

	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	})
	var body []byte
	for range 8 {
		clock.advance(idle * 3 / 4)
		next <- struct{}{}
		chunk := make([]byte, len("chunk"))
		if _, err := io.ReadFull(resp.Body, chunk); err != nil {
			t.Fatalf("after %q the next chunk failed with %v; want it within the idle time", body, err)
		}
		body = append(body, chunk...)
	}
	rest, err := io.ReadAll(resp.Body)

	if err != nil || string(body)+string(rest) != strings.Repeat("chunk", 8) {
		t.Errorf("read %q, %v; want every chunk and no error", string(body)+string(rest), err)
	}
}

// fakeTimer is an idle timer on a clock only the test moves, so a scheduler stall on a loaded runner cannot fire it.
type fakeTimer struct {
	mu     sync.Mutex
	d      time.Duration
	waited time.Duration
	fire   func()
}

func (f *fakeTimer) start(d time.Duration, fire func()) timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.d, f.fire = d, fire

	return f
}

func (f *fakeTimer) Reset(d time.Duration) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.d, f.waited = d, 0

	return true
}

func (f *fakeTimer) Stop() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fire = nil

	return true
}

// advance moves the clock by d, and fires the timer once the time since its last reset reaches its duration.
func (f *fakeTimer) advance(d time.Duration) {
	f.mu.Lock()
	f.waited += d
	fire := f.fire
	if f.waited < f.d {
		fire = nil
	}
	f.mu.Unlock()
	if fire != nil {
		fire()
	}
}
