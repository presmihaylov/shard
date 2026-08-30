package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newCA(t *testing.T) *CA {
	t.Helper()

	ca, err := LoadOrCreate(filepath.Join(t.TempDir(), "ca"))
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	return ca
}

func TestTheCAIsMintedOnceAndReadBack(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")

	first, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatalf("stat the key: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the key is mode %o, want 600", info.Mode().Perm())
	}

	again, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate again: %v", err)
	}
	if string(again.PEM()) != string(first.PEM()) {
		t.Error("a second LoadOrCreate minted a new CA instead of reading the first back")
	}
	if strings.Contains(string(first.PEM()), "PRIVATE") {
		t.Error("PEM carries the key")
	}
}

func TestALeafIsSignedByTheCAAndKept(t *testing.T) {
	ca := newCA(t)

	leaf, err := ca.Leaf("api.example.com")
	if err != nil {
		t.Fatalf("Leaf: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{DNSName: "api.example.com", Roots: pool}); err != nil {
		t.Errorf("the leaf does not verify against the CA: %v", err)
	}

	again, err := ca.Leaf("api.example.com")
	if err != nil {
		t.Fatalf("Leaf again: %v", err)
	}
	if again != leaf {
		t.Error("a second Leaf minted again instead of reusing the first")
	}
}

type fakeDirector struct {
	route  Route
	err    error
	source netip.Addr
	host   string
	port   int
}

func (f *fakeDirector) Route(_ context.Context, source netip.Addr, host string, port int) (Route, error) {
	f.source, f.host, f.port = source, host, port

	return f.route, f.err
}

// echo is an upstream that writes back what it was asked, so a test sees what the proxy sent.
func echo(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	_, _ = io.WriteString(w, r.Host+" "+r.URL.RequestURI()+" "+r.Header.Get("Authorization")+" "+string(body))
}

func serve(t *testing.T, s *Server, tlsSide bool) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	go func() {
		if tlsSide {
			_ = s.ServeTLS(l)

			return
		}
		_ = s.ServePlain(l)
	}()

	return l.Addr().String()
}

func targetOf(t *testing.T, srv *httptest.Server) netip.AddrPort {
	t.Helper()

	target, err := netip.ParseAddrPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("parse the upstream address: %v", err)
	}

	return target
}

func TestPlainRequestsGoToThePinnedTargetRewritten(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(echo))
	t.Cleanup(upstream.Close)

	director := &fakeDirector{route: Route{
		Target: targetOf(t, upstream),
		Rewrite: func(r *http.Request) error {
			r.Header.Set("Authorization", "Bearer real")

			return nil
		},
	}}

	addr := serve(t, New(newCA(t), director), false)

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+addr+"/v1?q=1", strings.NewReader("hello"))
	req.Host = "api.example.com:80"
	req.Header.Set("Authorization", "Bearer mock")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request through the proxy: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if want := "api.example.com:80 /v1?q=1 Bearer real hello"; string(body) != want {
		t.Errorf("the upstream saw %q, want %q", body, want)
	}
	if director.host != "api.example.com" || director.port != PlainPort || director.source != netip.MustParseAddr("127.0.0.1") {
		t.Errorf("the director was asked for %s:%d from %s", director.host, director.port, director.source)
	}
}

func TestADenialIs403WithTheReasonAndNothingIsSent(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	t.Cleanup(upstream.Close)

	director := &fakeDirector{err: &Denied{Reason: "policy locked denies evil.example.com"}}
	addr := serve(t, New(newCA(t), director), false)

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/", nil)
	req.Host = "evil.example.com"

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request through the proxy: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "policy locked denies") {
		t.Errorf("got %d %q", resp.StatusCode, body)
	}
	if called {
		t.Error("a denied request reached the upstream")
	}
}

func TestADirectorErrorIs502(t *testing.T) {
	director := &fakeDirector{err: errors.New("resolver down")}
	addr := serve(t, New(newCA(t), director), false)

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/", nil)
	req.Host = "api.example.com"

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request through the proxy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("got %d, want 502", resp.StatusCode)
	}
}

// The upstream presents a certificate from the same CA, so the test stays offline; in production the
// proxy verifies against the system roots.
func TestTLSIsTerminatedForTheNameTheClientAskedFor(t *testing.T) {
	ca := newCA(t)
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)

	upstreamLeaf, err := ca.Leaf("api.example.com")
	if err != nil {
		t.Fatalf("Leaf: %v", err)
	}
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(echo))
	upstream.TLS = &tls.Config{Certificates: []tls.Certificate{*upstreamLeaf}, MinVersion: tls.VersionTLS12}
	upstream.StartTLS()
	t.Cleanup(upstream.Close)

	director := &fakeDirector{route: Route{Target: targetOf(t, upstream)}}
	addr := serve(t, New(ca, director, WithRootCAs(pool)), true)

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}}

	resp, err := client.Get("https://api.example.com/secure")
	if err != nil {
		t.Fatalf("request through the proxy: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if want := "api.example.com /secure  "; string(body) != want {
		t.Errorf("the upstream saw %q, want %q", body, want)
	}
	if director.port != TLSPort || director.host != "api.example.com" {
		t.Errorf("the director was asked for %s:%d", director.host, director.port)
	}
}

func TestTLSWithoutAHostNameIsRefused(t *testing.T) {
	addr := serve(t, New(newCA(t), &fakeDirector{}), true)

	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}) // #nosec G402
	if err == nil {
		_ = conn.Close()
		t.Fatal("a handshake with no server name succeeded, so the proxy spoke for a host it could not name")
	}
}

func TestAHostHeaderThatDisagreesWithTheHandshakeIsRefused(t *testing.T) {
	ca := newCA(t)
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)

	addr := serve(t, New(ca, &fakeDirector{}), true)

	conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: pool, ServerName: "api.example.com", MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: other.example.com\r\n\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}

	reply := make([]byte, 512)
	n, _ := conn.Read(reply)
	if !strings.HasPrefix(string(reply[:n]), "HTTP/1.1 400") {
		t.Errorf("got %q, want a 400", reply[:n])
	}
}
