package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestLoadCAMintsOnceAndSignsALeafPerHost(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proxy")

	ca, err := LoadCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	again, err := LoadCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ca.CertPEM(), again.CertPEM()) {
		t.Error("a second load minted a second CA")
	}

	info, err := os.Stat(filepath.Join(dir, keyFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != keyPerm {
		t.Errorf("the CA key is %v, want %04o", info.Mode().Perm(), keyPerm)
	}

	leaf, err := ca.Leaf("api.example.com")
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.CertPEM())
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{DNSName: "api.example.com", Roots: pool}); err != nil {
		t.Errorf("the leaf does not verify for its host under the CA: %v", err)
	}

	same, err := ca.Leaf("api.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if same != leaf {
		t.Error("a second ask minted a second leaf")
	}
}

func TestLeafCacheStaysBounded(t *testing.T) {
	ca, err := LoadCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	for i := range leafCap + 5 {
		if _, err := ca.Leaf(strings.Repeat("h", i%50+1) + ".example.com"); err != nil {
			t.Fatal(err)
		}
	}
	// Names repeat, so the bound is the whole test: a cache that grew past it would hold more than distinct names.
	if len(ca.leaves) > leafCap || ca.order.Len() != len(ca.leaves) {
		t.Errorf("the cache holds %d leaves in a list of %d, want at most %d", len(ca.leaves), ca.order.Len(), leafCap)
	}
}

// fakeDirector allows every host but deny.test, sends everything to upstream, and swaps the placeholder.
type fakeDirector struct {
	upstream netip.AddrPort
	fail     error

	mu   sync.Mutex
	seen []Request
}

func (d *fakeDirector) Decide(_ context.Context, req Request) (Decision, error) {
	d.mu.Lock()
	d.seen = append(d.seen, req)
	d.mu.Unlock()

	if d.fail != nil {
		return Decision{}, d.fail
	}
	if req.Host == "deny.test" {
		return Decision{Rule: "deny deny.test tcp:80,443", Reason: "the policy names it"}, nil
	}

	return Decision{Allowed: true, Upstream: d.upstream}, nil
}

func (d *fakeDirector) Rewrite(_ context.Context, _ Request, out *http.Request, body []byte) ([]byte, error) {
	out.Header.Set("Authorization", strings.ReplaceAll(out.Header.Get("Authorization"), "mock-TOKEN", "real-TOKEN"))
	if body == nil {
		return nil, nil
	}

	return bytes.ReplaceAll(body, []byte("mock-TOKEN"), []byte("real-TOKEN")), nil
}

type harness struct {
	ca       *CA
	director *fakeDirector
	plain    net.Listener
	secure   net.Listener
	log      *bytes.Buffer
}

// newHarness runs the proxy over two loopback listeners in front of an upstream that echoes what it got.
func newHarness(t *testing.T, upstream http.Handler) *harness {
	t.Helper()

	ca, err := LoadCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	echo := httptest.NewServer(upstream)
	t.Cleanup(echo.Close)

	h := &harness{ca: ca, director: &fakeDirector{upstream: netip.MustParseAddrPort(echo.Listener.Addr().String())}, log: &bytes.Buffer{}}
	for _, l := range []*net.Listener{&h.plain, &h.secure} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		*l = listener
	}

	server, err := New(Config{Address: netip.MustParseAddr("127.0.0.1"), CA: ca, Director: h.director, Log: log.New(&syncWriter{buf: h.log}, "", 0)})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, h.plain, h.secure) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve ended with %v", err)
		}
	})

	return h
}

type syncWriter struct {
	mu  sync.Mutex
	buf *bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.buf.Write(p)
}

// client dials the proxy's listener for whatever name the URL carries, trusting the proxy CA over TLS.
func (h *harness) client() *http.Client {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(h.ca.CertPEM())

	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", h.plain.Addr().String())
		},
		DialTLSContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}

			return tls.Dial("tcp", h.secure.Addr().String(), &tls.Config{RootCAs: pool, ServerName: host, MinVersion: tls.VersionTLS12})
		},
	}}
}

// echoHandler answers with the method, path, host and headers it saw, and the body it read.
func echoHandler(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}
	w.Header().Set("X-Seen-Authorization", r.Header.Get("Authorization"))
	w.Header().Set("X-Seen-Host", r.Host)
	w.Header().Set("X-Seen-Length", r.Header.Get("Content-Length"))
	_, _ = w.Write(body)
}

func TestProxyForwardsWhatTheDirectorRewrote(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(echoHandler))

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://api.test/v1/chat", strings.NewReader(`{"key":"mock-TOKEN"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer mock-TOKEN")

	resp, err := h.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusOK || string(body) != `{"key":"real-TOKEN"}` {
		t.Errorf("the upstream got %d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Seen-Authorization") != "Bearer real-TOKEN" || resp.Header.Get("X-Seen-Host") != "api.test" {
		t.Errorf("the upstream saw %v", resp.Header)
	}
	if resp.Header.Get("X-Seen-Length") != "20" {
		t.Errorf("the upstream saw Content-Length %q, want the rewritten body's", resp.Header.Get("X-Seen-Length"))
	}

	seen := h.director.seen[0]
	if seen.Host != "api.test" || seen.Port != 80 || seen.TLS || seen.Source != netip.MustParseAddr("127.0.0.1") {
		t.Errorf("the director was asked about %+v", seen)
	}
	if log := h.log.String(); strings.Contains(log, "TOKEN") || !strings.Contains(log, "POST api.test:80 200") {
		t.Errorf("the log holds:\n%s", log)
	}
}

func TestProxyTerminatesTLSAndInsistsOnOneName(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(echoHandler))

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://deny.test/secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h.client().Do(req)
	if err != nil {
		t.Fatalf("the tls handshake with the proxy CA failed: %v", err)
	}
	defer resp.Body.Close()

	var denial map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&denial); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden || denial["rule"] != "deny deny.test tcp:80,443" || denial["host"] != "deny.test" || denial["port"] != "443" {
		t.Errorf("a denied request got %d %v", resp.StatusCode, denial)
	}
	if seen := h.director.seen[0]; !seen.TLS || seen.Port != 443 {
		t.Errorf("the director was asked about %+v, want a tls request on 443", seen)
	}

	// A Host header naming another host than the handshake did is a lie to one of them.
	req, err = http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.test/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "other.test"
	resp, err = h.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a host header that disagrees with the sni got %d, want 400", resp.StatusCode)
	}

	// Without a name the proxy has nothing to judge or to sign, so the handshake itself is refused.
	_, err = tls.Dial("tcp", h.secure.Addr().String(), &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}) //nolint:gosec // the refusal is the point
	if err == nil {
		t.Error("a handshake with no server name went through")
	}
}

func TestProxyAnswers502WhenTheDirectorCannotJudge(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(echoHandler))
	h.director.fail = errors.New("no sandbox holds the address")

	resp, err := h.client().Get("http://api.test/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("a director error got %d, want 502", resp.StatusCode)
	}
}

func TestProxyStreamsABodyPastTheCapUnchanged(t *testing.T) {
	var got int64
	var placeholderSeen bool
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}
		got = int64(len(body))
		placeholderSeen = bytes.Contains(body, []byte("mock-TOKEN"))
	}))

	large := append(bytes.Repeat([]byte("x"), BodyCap), []byte("mock-TOKEN")...)
	resp, err := h.client().Post("http://api.test/upload", "application/octet-stream", bytes.NewReader(large))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK || got != int64(len(large)) {
		t.Errorf("the upstream got %d with %d bytes, want 200 with %d", resp.StatusCode, got, len(large))
	}
	if !placeholderSeen {
		t.Error("a body past the cap was rewritten, want it streamed as it was")
	}
}
