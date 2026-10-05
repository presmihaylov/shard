package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
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

// fakeDirector allows every host but deny.test, holds every body but stream.test's, sends everything to upstream, and swaps the placeholder in every header and the body.
type fakeDirector struct {
	upstream netip.AddrPort
	fail     error
	// resolve is how long a decision takes, as a lookup that waits on external DNS does.
	resolve time.Duration
	// deciding gets the context of each decision as it starts.
	deciding chan context.Context
	// grow is what each rewrite reserves, as a value longer than its placeholder does.
	grow int

	mu   sync.Mutex
	seen []Request
}

func (d *fakeDirector) Decide(ctx context.Context, req Request) (Decision, error) {
	d.mu.Lock()
	d.seen = append(d.seen, req)
	d.mu.Unlock()

	if d.deciding != nil {
		d.deciding <- ctx
	}
	if d.fail != nil {
		return Decision{}, d.fail
	}
	if d.resolve > 0 {
		select {
		case <-ctx.Done():
			return Decision{}, ctx.Err()
		case <-time.After(d.resolve):
		}
	}
	if req.Host == "deny.test" {
		return Decision{Rule: "deny deny.test tcp:80,443", Reason: "the policy names it"}, nil
	}

	return Decision{Allowed: true, Upstream: d.upstream, Hold: req.Host != "stream.test"}, nil
}

func (d *fakeDirector) Rewrite(_ context.Context, _ Request, out *http.Request, body []byte, reserve Reserve) ([]byte, error) {
	if err := reserve(d.grow); err != nil {
		return nil, fmt.Errorf("put the secrets in: %w", err)
	}
	for _, values := range out.Header {
		for i, v := range values {
			values[i] = strings.ReplaceAll(v, "mock-TOKEN", "real-TOKEN")
		}
	}
	if body == nil {
		return nil, nil
	}

	return bytes.ReplaceAll(body, []byte("mock-TOKEN"), []byte("real-TOKEN")), nil
}

type harness struct {
	ca       *CA
	director *fakeDirector
	server   *Server
	plain    net.Listener
	secure   net.Listener
	log      *syncWriter
	// stop ends Serve and returns what it returned; a second call returns the same.
	stop func() error
}

// newHarness runs the proxy over two loopback listeners in front of an upstream that echoes what it got; each tune edits the server before it serves.
func newHarness(t *testing.T, upstream http.Handler, tunes ...func(*Server)) *harness {
	t.Helper()

	ca, err := LoadCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	echo := httptest.NewServer(upstream)
	t.Cleanup(echo.Close)

	h := &harness{ca: ca, director: &fakeDirector{upstream: netip.MustParseAddrPort(echo.Listener.Addr().String())}, log: &syncWriter{buf: &bytes.Buffer{}}}
	for _, l := range []*net.Listener{&h.plain, &h.secure} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		*l = listener
	}

	server, err := New(Config{Address: netip.MustParseAddr("127.0.0.1"), CA: ca, Director: h.director, Log: log.New(h.log, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	h.server = server

	for _, tune := range tunes {
		tune(server)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, h.plain, h.secure) }()
	h.stop = sync.OnceValue(func() error {
		cancel()
		return <-done
	})
	t.Cleanup(func() {
		if err := h.stop(); err != nil {
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

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.buf.String()
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

// SHARD-345: each request logs its host, so a host past a DNS name is refused unprinted, and a megabyte one never reaches the handler.
func TestProxyRefusesAHostLongerThanADNSName(t *testing.T) {
	atTheBound := strings.Repeat(strings.Repeat("a", 62)+".", 4) + "a"
	for name, tc := range map[string]struct {
		host   string
		status int
	}{
		"a dns name at the bound": {atTheBound, http.StatusOK},
		"one byte past it":        {atTheBound + "a", http.StatusBadRequest},
		"a megabyte":              {strings.Repeat("a", 1_000_000), http.StatusRequestHeaderFieldsTooLarge},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, http.HandlerFunc(echoHandler))

			status, body := sendRaw(t, h, http.MethodGet, tc.host)
			if status != tc.status {
				t.Fatalf("a %d-byte host got %d %s, want %d", len(tc.host), status, body, tc.status)
			}
			if tc.status == http.StatusOK {
				if seen := h.director.seen[0].Host; seen != tc.host {
					t.Errorf("the director was asked about %q, want the host at the bound", seen)
				}

				return
			}

			if len(h.director.seen) != 0 {
				t.Errorf("the director was asked about a %d-byte host", len(tc.host))
			}
			if strings.Contains(body, tc.host) {
				t.Error("the answer echoes the host")
			}
			if log := h.log.String(); len(log) >= 1<<10 || strings.Contains(log, tc.host) {
				t.Errorf("a %d-byte host left %d log bytes, want under 1 KiB and no host:\n%.300s", len(tc.host), len(log), log)
			}
		})
	}
}

func TestProxyClipsTheMethodItLogs(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(echoHandler))
	method := strings.Repeat("M", 10_000)

	if status, _ := sendRaw(t, h, method, "deny.test"); status != http.StatusForbidden {
		t.Fatalf("a denied request got %d, want 403", status)
	}

	log := h.log.String()
	if !strings.Contains(log, strings.Repeat("M", maxHostLen)+"... deny.test:80 denied") || strings.Contains(log, strings.Repeat("M", maxHostLen+1)) {
		t.Errorf("the log holds %d bytes, want the method cut at %d:\n%.300s", len(log), maxHostLen, log)
	}
}

func TestProxyRefusesATLSServerNameLongerThanADNSName(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(echoHandler))
	name := strings.Repeat(strings.Repeat("a", 62)+".", 64) + "test"

	conn, err := tls.Dial("tcp", h.secure.Addr().String(), &tls.Config{ServerName: name, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}) //nolint:gosec // the refusal is the point
	if err == nil {
		conn.Close()
		t.Fatal("a handshake for a server name past a dns name went through")
	}

	// The server logs the handshake error on its own goroutine, after the client already saw it.
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(h.log.String(), "longer than a dns name") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if log := h.log.String(); !strings.Contains(log, "longer than a dns name") || strings.Contains(log, name) {
		t.Errorf("the log holds:\n%.300s", log)
	}
}

// SHARD-347: a guest's denied requests and broken handshakes are held at its log bound and counted, and every answered request still logs.
func TestAFloodOfDeniedRequestsIsHeldAtTheLogBound(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(echoHandler))
	client := h.client()

	for _, url := range append(slices.Repeat([]string{"http://deny.test/"}, 100), slices.Repeat([]string{"http://api.test/"}, 20)...) {
		resp, err := client.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatal(err)
		}
	}

	log := awaitAccounted(t, h, "denied by", 100)
	if denied := strings.Count(log, "denied by"); denied > 12 {
		t.Errorf("100 denied requests wrote %d deny lines, want the burst", denied)
	}
	if answered := strings.Count(log, "GET api.test:80 200"); answered != 20 {
		t.Errorf("20 answered requests wrote %d answer lines, want every one", answered)
	}
}

func TestAFloodOfBrokenHandshakesIsHeldAtTheLogBound(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(echoHandler))
	name := strings.Repeat("a", maxHostLen+1)

	for range 50 {
		conn, err := tls.Dial("tcp", h.secure.Addr().String(), &tls.Config{ServerName: name, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}) //nolint:gosec // the refusal is the point
		if err == nil {
			conn.Close()
			t.Fatal("a handshake for a server name past a dns name went through")
		}
	}

	log := awaitAccounted(t, h, "TLS handshake error", 50)
	if failed := strings.Count(log, "TLS handshake error"); failed > 12 {
		t.Errorf("50 broken handshakes wrote %d lines, want the burst", failed)
	}
}

// awaitAccounted waits until the lines with marker and the counts of held lines add up to want, and answers the log.
func awaitAccounted(t *testing.T, h *harness, marker string, want int) string {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		log := h.log.String()
		accounted := strings.Count(log, marker)
		for line := range strings.SplitSeq(log, "\n") {
			var held int
			if _, err := fmt.Sscanf(line, "proxy: 127.0.0.1: held back %d lines past the log bound", &held); err == nil {
				accounted += held
			}
		}
		if accounted == want {
			return log
		}
		if time.Now().After(deadline) {
			t.Fatalf("the log accounts for %d of %d lines:\n%.1000s", accounted, want, log)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNamedSourceReadsTheGuestNetHTTPNames(t *testing.T) {
	for line, want := range map[string]netip.Addr{
		"http: TLS handshake error from 10.0.0.2:5555: EOF\n":        netip.MustParseAddr("10.0.0.2"),
		"http: TLS handshake error from [fd00::2]:5555: EOF\n":       netip.MustParseAddr("fd00::2"),
		"http: TLS handshake error from 10.0.0.2:5555: from 1.2.3.4": netip.MustParseAddr("10.0.0.2"),
		"http: Accept error: too many open files; retrying in 5ms\n": {},
		"http: TLS handshake error from nowhere: EOF\n":              {},
	} {
		if got := namedSource(line); got != want {
			t.Errorf("namedSource(%q) = %v, want %v", line, got, want)
		}
	}
}

// sendRaw writes one request with the method and host as the guest chose them, and reads the answer while it writes, since the proxy may answer a head it refused before it read all of it.
func sendRaw(t *testing.T, h *harness, method, host string) (int, string) {
	t.Helper()

	conn, err := net.Dial("tcp", h.plain.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}

	head := fmt.Sprintf("%s / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", method, host)
	written := make(chan error, 1)
	go func() {
		_, err := io.WriteString(conn, head)
		written <- err
	}()
	t.Cleanup(func() {
		conn.Close()
		// A head past the header bound is cut off mid-write, which is the refusal; a shorter one must go out whole.
		if err := <-written; err != nil && len(head) < maxHeaderBytes {
			t.Errorf("write the request: %v", err)
		}
	})

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer to a %d-byte head: %v", len(head), err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}

	return resp.StatusCode, string(body)
}

// A failed upstream answers the same fixed 502 whatever the director put in the request, since its error can quote it.
func TestProxyNeverEchoesTheRewrittenRequestInA502(t *testing.T) {
	for name, tc := range map[string]struct {
		upgrade  string
		upstream http.HandlerFunc
	}{
		// A tab is a valid header byte but no protocol name, so the reverse proxy refuses it before it dials.
		"an upgrade the proxy refuses": {"x\tmock-TOKEN", echoHandler},
		"an upgrade the upstream answers with another": {"mock-TOKEN", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Connection", "Upgrade")
			w.Header().Set("Upgrade", "other")
			w.WriteHeader(http.StatusSwitchingProtocols)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, tc.upstream)

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://api.test/", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", tc.upgrade)

			resp, err := h.client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}

			if resp.StatusCode != http.StatusBadGateway || string(body) != `{"error":"the request to the upstream failed"}`+"\n" {
				t.Errorf("the guest got %d %s, want the fixed 502", resp.StatusCode, body)
			}
			if log := h.log.String(); strings.Contains(log, "real-TOKEN") {
				t.Errorf("the log holds the value:\n%s", log)
			}
		})
	}
}

// An upstream's headers past maxResponseHeaderBytes get the fixed 502, so no connection holds the transport's 10 MiB default.
func TestProxyBoundsTheHeadersOfAnUpstream(t *testing.T) {
	for name, tc := range map[string]struct {
		size int
		want int
	}{
		"half the bound": {maxResponseHeaderBytes / 2, http.StatusOK},
		"the bound":      {maxResponseHeaderBytes, http.StatusBadGateway},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-Large", strings.Repeat("a", tc.size))
			}))

			if got := getStatus(t, h); got != tc.want {
				t.Errorf("a %d byte header got %d, want %d", tc.size, got, tc.want)
			}
		})
	}
}

func TestProxyGivesUpOnAnUpstreamThatSendsNoHeaders(t *testing.T) {
	previous := responseHeaderTimeout
	responseHeaderTimeout = 100 * time.Millisecond
	t.Cleanup(func() { responseHeaderTimeout = previous })

	h := newHarness(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))

	start := time.Now()
	if got := getStatus(t, h); got != http.StatusBadGateway {
		t.Errorf("an upstream that sent no headers got %d, want 502", got)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Errorf("the proxy waited %s on the upstream's headers", waited)
	}
}

// getStatus sends one GET through the proxy and returns the status the guest got.
func getStatus(t *testing.T, h *harness) int {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://api.test/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode
}

// A held body takes the value up to BodyCap, whether it names its length or not; past the cap it goes as it was.
func TestProxyRewritesAHeldBodyUpToTheCap(t *testing.T) {
	small := []byte(`{"key":"mock-TOKEN"}`)
	large := append(bytes.Repeat([]byte("x"), BodyCap), []byte("mock-TOKEN")...)
	unsized := func(b []byte) io.Reader { return struct{ io.Reader }{bytes.NewReader(b)} }

	for name, tc := range map[string]struct {
		body io.Reader
		want []byte
	}{
		"under the cap with no length": {unsized(small), []byte(`{"key":"real-TOKEN"}`)},
		"past the cap with a length":   {bytes.NewReader(large), large},
		"past the cap with no length":  {unsized(large), large},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, http.HandlerFunc(echoHandler))

			resp, err := h.client().Post("http://api.test/upload", "application/octet-stream", tc.body)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}

			if resp.StatusCode != http.StatusOK || !bytes.Equal(got, tc.want) {
				t.Errorf("the upstream got %d with %d bytes, want 200 with %d", resp.StatusCode, len(got), len(tc.want))
			}
		})
	}
}

// A body the director need not hold reaches the upstream as it arrives, so it costs no memory however long it stalls (SHARD-348).
func TestProxyStreamsABodyItNeedNotHold(t *testing.T) {
	first := make(chan string, 1)
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		head := make([]byte, len("mock-"))
		if _, err := io.ReadFull(r.Body, head); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}
		first <- string(head)
		rest, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}
		_, _ = w.Write(append(head, rest...))
	}))

	conn := h.dial(t, false)
	write(t, conn, "POST /up HTTP/1.1\r\nHost: stream.test\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nmock-\r\n")
	select {
	case head := <-first:
		if head != "mock-" {
			t.Errorf("the upstream got %q first", head)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream got nothing before the body ended")
	}
	write(t, conn, "5\r\nTOKEN\r\n0\r\n\r\n")

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "mock-TOKEN" {
		t.Errorf("the upstream got %d %s, want the body as it was", resp.StatusCode, body)
	}
}

// A held body that stops arriving answers 408 and closes its connection, so it pins no memory (SHARD-348).
func TestProxyClosesAHeldBodyThatStalls(t *testing.T) {
	previous := heldReadTimeout
	heldReadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { heldReadTimeout = previous })

	for name, secure := range map[string]bool{"in cleartext": false, "over tls": true} {
		t.Run(name, func(t *testing.T) {
			arrived := make(chan struct{}, 1)
			h := newHarness(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { arrived <- struct{}{} }))

			conn := h.dial(t, secure)
			write(t, conn, "POST /up HTTP/1.1\r\nHost: api.test\r\nContent-Length: 600\r\n\r\n"+strings.Repeat("x", 300))
			start := time.Now()
			if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			answer, err := io.ReadAll(conn)
			if err != nil {
				t.Fatalf("the connection stayed open: %v, after:\n%s", err, answer)
			}

			if !strings.HasPrefix(string(answer), "HTTP/1.1 408") || !strings.Contains(string(answer), "did not arrive within 200ms") {
				t.Errorf("the stalled body got:\n%s", answer)
			}
			if waited := time.Since(start); waited > 2*time.Second {
				t.Errorf("the connection closed %s after the body stalled", waited)
			}
			select {
			case <-arrived:
				t.Error("the upstream got a request whose body stalled")
			default:
			}
			if held := h.held(); held != 0 {
				t.Errorf("the stalled body still holds %d bytes of the budget", held)
			}
		})
	}
}

// One sandbox holds at most heldBudget of bodies at once; past it a request answers 503 and holds nothing (SHARD-348).
func TestProxyHoldsABudgetOfBodiesPerSandbox(t *testing.T) {
	previous := heldBudget
	heldBudget = 1000
	t.Cleanup(func() { heldBudget = previous })
	h := newHarness(t, http.HandlerFunc(echoHandler))
	post := func() (int, string) {
		resp, err := h.client().Post("http://api.test/b", "application/octet-stream", strings.NewReader(strings.Repeat("y", 600)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}

		return resp.StatusCode, string(body)
	}

	stalled := h.dial(t, false)
	write(t, stalled, "POST /a HTTP/1.1\r\nHost: api.test\r\nContent-Length: 600\r\n\r\n"+strings.Repeat("x", 300))
	h.waitHeld(t, 600)

	if code, body := post(); code != http.StatusServiceUnavailable || !strings.Contains(body, "budget of 1000 bytes") {
		t.Errorf("a body past the budget got %d %s, want a 503 that names it", code, body)
	}

	write(t, stalled, strings.Repeat("x", 300))
	resp, err := http.ReadResponse(bufio.NewReader(stalled), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("the held body got %d once it ended, want 200", resp.StatusCode)
	}

	h.waitHeld(t, 0)
	if code, body := post(); code != http.StatusOK {
		t.Errorf("a body after the budget came back got %d %s, want 200", code, body)
	}
}

// What a value adds is charged with the body until the request is forwarded, so a rewrite past the budget is a 503 that sends nothing (SHARD-348).
func TestProxyChargesWhatARewriteReservesUntilItIsForwarded(t *testing.T) {
	previous := heldBudget
	heldBudget = 1000
	t.Cleanup(func() { heldBudget = previous })
	forwarding, release := make(chan struct{}), make(chan struct{})
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarding <- struct{}{}
		<-release
		echoHandler(w, r)
	}))
	post := func(answer chan<- int) {
		resp, err := h.client().Post("http://api.test/b", "application/octet-stream", strings.NewReader(strings.Repeat("y", 300)))
		if err != nil {
			t.Error(err)
			answer <- 0

			return
		}
		defer resp.Body.Close()
		answer <- resp.StatusCode
	}

	h.director.grow = 800
	answer := make(chan int, 1)
	go post(answer)
	select {
	case code := <-answer:
		if code != http.StatusServiceUnavailable {
			t.Errorf("a rewrite past the budget got %d, want 503", code)
		}
	case <-forwarding:
		t.Fatal("a rewrite past the budget reached the upstream")
	}
	h.waitHeld(t, 0)

	h.director.grow = 600
	go post(answer)
	<-forwarding
	if held := h.held(); held != 900 {
		t.Errorf("the budget holds %d bytes while the request is forwarded, want the body and the rewrite, 900", held)
	}
	close(release)
	if code := <-answer; code != http.StatusOK {
		t.Errorf("a rewrite inside the budget got %d, want 200", code)
	}
	h.waitHeld(t, 0)
}

// A fire-and-forget client half-closes once its request is written, and net/http cancels the request context on that EOF (SHARD-238).
func TestProxyFinishesARequestAfterTheClientHalfCloses(t *testing.T) {
	arrived := make(chan string, 1)
	h := newHarness(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		arrived <- r.URL.Path
	}))
	h.director.resolve = 200 * time.Millisecond

	if answer := sendAndHalfClose(t, h); !strings.HasPrefix(answer, "HTTP/1.1 200") {
		t.Errorf("the half-closed client got:\n%s", answer)
	}
	select {
	case path := <-arrived:
		if path != "/ping" {
			t.Errorf("the upstream got %s, want /ping", path)
		}
	default:
		t.Error("the upstream never got the request")
	}
}

func TestProxyGivesUpOnAGoneClientAfterTheGrace(t *testing.T) {
	previous := clientGoneGrace
	clientGoneGrace = 100 * time.Millisecond
	t.Cleanup(func() { clientGoneGrace = previous })

	h := newHarness(t, http.HandlerFunc(echoHandler))
	h.director.resolve = 5 * time.Second

	start := time.Now()
	if answer := sendAndHalfClose(t, h); !strings.HasPrefix(answer, "HTTP/1.1 502") {
		t.Errorf("a decision past the grace got:\n%s", answer)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Errorf("the proxy held the request %s after its client went", waited)
	}
}

// The grace is the client's, never the server's: a stopped proxy cuts a request its client left at once.
func TestProxyStopCutsARequestItsClientLeft(t *testing.T) {
	previous := shutdownGrace
	shutdownGrace = 100 * time.Millisecond
	t.Cleanup(func() { shutdownGrace = previous })
	h := newHarness(t, http.HandlerFunc(echoHandler))
	h.director.resolve = time.Minute
	h.director.deciding = make(chan context.Context, 1)

	halfClose(t, h)
	var decision context.Context
	select {
	case decision = <-h.director.deciding:
	case <-time.After(5 * time.Second):
		t.Fatal("the proxy never asked the director")
	}
	if err := h.stop(); err != nil {
		t.Fatalf("Serve ended with %v", err)
	}

	select {
	case <-decision.Done():
	case <-time.After(time.Second):
		t.Error("the decision outlived the stopped proxy")
	}
}

// halfClose writes one request and closes the write side, as a fire-and-forget client does.
func halfClose(t *testing.T, h *harness) net.Conn {
	t.Helper()

	conn, err := net.Dial("tcp", h.plain.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := io.WriteString(conn, "GET /ping HTTP/1.1\r\nHost: api.test\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		t.Fatalf("the dial gave a %T", conn)
	}
	if err := tcp.CloseWrite(); err != nil {
		t.Fatal(err)
	}

	return conn
}

// sendAndHalfClose half-closes one request and reads what comes back.
func sendAndHalfClose(t *testing.T, h *harness) string {
	t.Helper()

	conn := halfClose(t, h)
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	answer, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}

	return string(answer)
}

// dial opens one connection to the proxy, over tls for api.test when secure.
func (h *harness) dial(t *testing.T, secure bool) net.Conn {
	t.Helper()

	conn, err := net.Dial("tcp", h.plain.Addr().String())
	if secure {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(h.ca.CertPEM())
		conn, err = tls.Dial("tcp", h.secure.Addr().String(), &tls.Config{RootCAs: pool, ServerName: "api.test", MinVersion: tls.VersionTLS12})
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	return conn
}

func write(t *testing.T, conn net.Conn, s string) {
	t.Helper()

	if _, err := io.WriteString(conn, s); err != nil {
		t.Fatal(err)
	}
}

// held is what the loopback source holds of the budget now.
func (h *harness) held() int {
	h.server.held.mu.Lock()
	defer h.server.held.mu.Unlock()

	return h.server.held.by[netip.MustParseAddr("127.0.0.1")]
}

func (h *harness) waitHeld(t *testing.T, want int) {
	t.Helper()

	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if h.held() == want {
			return
		}
	}
	t.Fatalf("the budget holds %d bytes, want %d", h.held(), want)
}
