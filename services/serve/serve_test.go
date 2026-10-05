package serve

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/golang-jwt/jwt/v5"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
)

const testSecret = "e2e-secret-value-0000000000000000"

// upstream is a fake daemon on a socket under a root, which counts what reached it.
type upstream struct {
	root     string
	dialed   *atomic.Int64
	requests chan string
}

// fakeDaemon answers every request with 200 and its own body, echoes on a WebSocket, and counts the connections it accepted.
func fakeDaemon(t *testing.T) *upstream {
	return daemon(t, true)
}

// plainDaemon answers every request with 200, even a WebSocket handshake, so the front's non-101 path is exercised.
func plainDaemon(t *testing.T) *upstream {
	return daemon(t, false)
}

// daemon starts a fake daemon on a socket under a root and counts what reaches it. When upgrade is true it answers a WebSocket handshake with 101 and echoes; when false it answers every request with 200.
func daemon(t *testing.T, upgrade bool) *upstream {
	t.Helper()

	root := shortRoot(t)
	up := &upstream{root: root, dialed: &atomic.Int64{}, requests: make(chan string, 8)}

	listener, err := net.Listen("unix", filepath.Join(root, "shard.sock"))
	if err != nil {
		t.Fatalf("listen on the fake socket: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	server := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case up.requests <- r.Method + " " + r.URL.Path + " auth=" + r.Header.Get("Authorization"):
			default:
			}
			if upgrade && r.Header.Get("Upgrade") == "websocket" {
				echo(t, w, r)

				return
			}
			fmt.Fprintln(w, `{"sandboxes":[]}`)
		}),
	}
	server.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			up.dialed.Add(1)
		}
	}

	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })

	return up
}

// echo answers a WebSocket handshake and sends one message back as it came, then closes as the daemon does.
func echo(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		t.Errorf("accept the WebSocket: %v", err)

		return
	}
	defer conn.CloseNow()

	kind, payload, err := conn.Read(r.Context())
	if err != nil {
		t.Errorf("read the message: %v", err)

		return
	}
	if err := conn.Write(r.Context(), kind, payload); err != nil {
		t.Errorf("echo the message: %v", err)

		return
	}
	// Close writes the close frame the client asserts on before it blocks on the reply; the client's teardown races that read-back, so log it, never fail on it.
	if err := conn.Close(websocket.StatusNormalClosure, "echoed"); err != nil {
		t.Logf("the fake daemon read no close reply back: %v", err)
	}
}

// front starts a server over the fake daemon and answers the address it bound.
func front(t *testing.T, root, secret string) string {
	t.Helper()

	return frontWith(t, root, secret, func(_ *Server, listener net.Listener) net.Listener { return listener })
}

// frontWith is front, with tune to change the server and to pick the listener it serves.
func frontWith(t *testing.T, root, secret string, tune func(*Server, net.Listener) net.Listener) string {
	t.Helper()

	server, err := New(Config{Listen: "127.0.0.1:0", SigningKeyFile: secret, Root: root, Out: io.Discard})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	listener, err := server.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	ended := make(chan error, 1)
	served := tune(server, listener)
	go func() { ended <- server.Serve(ctx, served) }()

	t.Cleanup(func() {
		cancel()
		if err := <-ended; err != nil {
			t.Errorf("the front ended with %v", err)
		}
	})

	return listener.Addr().String()
}

// ask sends one request to the front, with the bearer token when it is not empty.
func ask(t *testing.T, address, token string) *http.Response {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+address+"/v0/sandboxes", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("ask the front: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	return resp
}

// askRoute sends one request of method to path on the front, with the bearer token when it is not empty.
func askRoute(t *testing.T, address, token, method, path string) *http.Response {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+address+path, nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("ask the front: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	return resp
}

// drain collects what a channel holds, gives a late sender a short moment, and answers the list.
func drain(ch chan string) []string {
	var got []string
	for {
		select {
		case s := <-ch:
			got = append(got, s)
		case <-time.After(200 * time.Millisecond):
			return got
		}
	}
}

func TestTheFrontSplicesAnAuthorizedRequestOntoTheSocket(t *testing.T) {
	up := fakeDaemon(t)
	env := newTokenEnv(t)
	token := mint(t, env, "ci")

	resp := ask(t, front(t, up.root, env.secret), token) //nolint:bodyclose // ask closes the body in a cleanup

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the front answered %d, want 200", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	if got := strings.TrimSpace(string(body)); got != `{"sandboxes":[]}` {
		t.Errorf("the front answered %q, want what the daemon wrote", got)
	}

	select {
	case got := <-up.requests:
		// The front rewrites only the Connection header, so the method, the path and the token reach the daemon.
		if want := "GET /v0/sandboxes auth=Bearer " + token; got != want {
			t.Errorf("the daemon saw %q, want %q", got, want)
		}
	default:
		t.Error("the daemon saw no request")
	}
}

// The front carries every message of a WebSocket both ways, because it stops parsing at the head.
func TestTheFrontSplicesAWebSocket(t *testing.T) {
	up := fakeDaemon(t)
	env := newTokenEnv(t)
	address := front(t, up.root, env.secret)
	token := mint(t, env, "ci")

	header := http.Header{"Authorization": {"Bearer " + token}}
	conn, _, err := websocket.Dial(t.Context(), "ws://"+address+"/v0/sandboxes/sandbox1/logs?follow=true", &websocket.DialOptions{HTTPHeader: header}) //nolint:bodyclose // a 101 has no body to close
	if err != nil {
		t.Fatalf("dial through the front: %v", err)
	}
	defer conn.CloseNow()

	if err := conn.Write(t.Context(), websocket.MessageBinary, []byte{1, 'u', 'p'}); err != nil {
		t.Fatalf("write through the front: %v", err)
	}

	kind, payload, err := conn.Read(t.Context())
	if err != nil {
		t.Fatalf("read through the front: %v", err)
	}
	if kind != websocket.MessageBinary || string(payload) != "\x01up" {
		t.Errorf("the echo came back as %v %q", kind, payload)
	}

	var closed websocket.CloseError
	if _, _, err := conn.Read(t.Context()); !errors.As(err, &closed) || closed.Code != websocket.StatusNormalClosure || closed.Reason != "echoed" {
		t.Errorf("the session ended with %v, want the daemon's close 1000 with its reason", err)
	}

	_, _, err = websocket.Dial(t.Context(), "ws://"+address+"/v0/sandboxes/sandbox1/logs?follow=true", nil) //nolint:bodyclose // a refused dial has no body to close
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("a dial with no token got %v, want the 401 of the front", err)
	}
}

// The bare-LF reject is on the head only: a valid CRLF upgrade still splices, and body bytes that carry LF pass through untouched.
func TestTheFrontSplicesBodyBytesThatCarryLineFeeds(t *testing.T) {
	up := fakeDaemon(t)
	env := newTokenEnv(t)
	address := front(t, up.root, env.secret)
	token := mint(t, env, "ci")

	header := http.Header{"Authorization": {"Bearer " + token}}
	conn, _, err := websocket.Dial(t.Context(), "ws://"+address+"/v0/sandboxes/sandbox1/logs?follow=true", &websocket.DialOptions{HTTPHeader: header}) //nolint:bodyclose // a 101 has no body to close
	if err != nil {
		t.Fatalf("dial through the front: %v", err)
	}
	defer conn.CloseNow()

	body := []byte{1, 'a', '\n', 'b', '\r', '\n', '\n', 'c'}
	if err := conn.Write(t.Context(), websocket.MessageBinary, body); err != nil {
		t.Fatalf("write through the front: %v", err)
	}

	kind, payload, err := conn.Read(t.Context())
	if err != nil {
		t.Fatalf("read through the front: %v", err)
	}
	if kind != websocket.MessageBinary || string(payload) != string(body) {
		t.Errorf("the echo came back as %v %q, want the LF-carrying body unchanged", kind, payload)
	}

	var closed websocket.CloseError
	if _, _, err := conn.Read(t.Context()); !errors.As(err, &closed) || closed.Code != websocket.StatusNormalClosure {
		t.Errorf("the session ended with %v, want the daemon's normal close", err)
	}
}

func TestABadTokenIs401AndNothingIsDialed(t *testing.T) {
	up := fakeDaemon(t)
	address := front(t, up.root, secretFile(t, testSecret))

	now := time.Now()
	valid := jwt.RegisteredClaims{Subject: "ci", IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour))}

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate an rsa key: %v", err)
	}

	tokens := map[string]string{
		"empty":        "",
		"not a jwt":    "not-a-jwt",
		"wrong secret": signed(t, jwt.SigningMethodHS256, []byte("another-secret-value-000000000000"), valid),
		"alg none":     signed(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, valid),
		"alg rs256":    signed(t, jwt.SigningMethodRS256, rsaKey, valid),
		"expired":      signed(t, jwt.SigningMethodHS256, []byte(testSecret), jwt.RegisteredClaims{Subject: "ci", IssuedAt: jwt.NewNumericDate(now.Add(-2 * time.Hour)), ExpiresAt: jwt.NewNumericDate(now.Add(-time.Hour))}),
		"no subject":   signed(t, jwt.SigningMethodHS256, []byte(testSecret), jwt.RegisteredClaims{IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour))}),
		"no expiry":    signed(t, jwt.SigningMethodHS256, []byte(testSecret), jwt.RegisteredClaims{Subject: "ci", IssuedAt: jwt.NewNumericDate(now)}),
	}

	for name, token := range tokens {
		resp := ask(t, address, token) //nolint:bodyclose // ask closes the body in a cleanup
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: the front answered %d, want 401", name, resp.StatusCode)
		}

		var body struct {
			Error struct {
				Code    models.Code `json:"code"`
				Message string      `json:"message"`
			} `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("%s: decode the refusal: %v", name, err)
		}
		if body.Error.Code != models.CodeUnauthorized || body.Error.Message == "" {
			t.Errorf("%s: the refusal reads %+v, want a line and the code unauthorized", name, body)
		}
	}

	if dialed := up.dialed.Load(); dialed != 0 {
		t.Errorf("the front dialed the socket %d times for requests it refused, want none", dialed)
	}
}

func TestMintAndVerifyRoundTrip(t *testing.T) {
	token, err := Mint([]byte(testSecret), "ci", []string{"sandbox:read", "exec"}, time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	verified, err := verify([]byte(testSecret), token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if verified.ID == "" || verified.ExpiresAt == nil {
		t.Fatal("verify lost the signed token id or expiry")
	}
	if verified.Subject != "ci" {
		t.Errorf("verify answered %q, want the subject the token names", verified.Subject)
	}
	if strings.Join(verified.Scopes, ",") != "sandbox:read,exec" {
		t.Errorf("verify answered scopes %v, want the ones the token carries", verified.Scopes)
	}
}

func TestMintDefaultsToTheEveryVerbScope(t *testing.T) {
	token, err := Mint([]byte(testSecret), "ci", nil, time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	verified, err := verify([]byte(testSecret), token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if strings.Join(verified.Scopes, ",") != "*" {
		t.Errorf("verify answered scopes %v, want the default [\"*\"] Mint writes", verified.Scopes)
	}
}

// A token minted before SHARD-195 carries no scopes claim; the front still reads it as every verb.
func TestVerifyReadsAnAbsentScopesClaimAsEveryVerb(t *testing.T) {
	now := time.Now()
	token := signed(t, jwt.SigningMethodHS256, []byte(testSecret),
		jwt.RegisteredClaims{ID: "a-token-id", Subject: "ci", IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour))})

	verified, err := verify([]byte(testSecret), token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(verified.Scopes) != 0 {
		t.Errorf("verify answered scopes %v, want none, which is every verb", verified.Scopes)
	}
}

func TestMintRefusesAnEmptySubjectAndANegativeDuration(t *testing.T) {
	if _, err := Mint([]byte(testSecret), "", nil, time.Hour); err == nil {
		t.Error("Mint signed a token with no subject")
	}
	if _, err := Mint([]byte(testSecret), "ci", nil, -time.Hour); err == nil {
		t.Error("Mint signed a token with a negative duration")
	}
	// A duration of zero is valid now: it mints a token that never expires.
	if _, err := Mint([]byte(testSecret), "ci", nil, 0); err != nil {
		t.Errorf("Mint refused a zero duration, which should mint a token that never expires: %v", err)
	}
}

// MintToken's record carries the scopes asked and an expires_at equal to the token's exp, to the second.
func TestMintTokenRecordAgreesWithTheToken(t *testing.T) {
	minted, err := MintToken([]byte(testSecret), "ci", []string{"sandbox:read"}, time.Hour)
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	if strings.Join(minted.Scopes, ",") != "sandbox:read" {
		t.Errorf("the record carries scopes %v, want the ones asked", minted.Scopes)
	}
	if minted.ExpiresAt == nil {
		t.Fatal("the record carries no expires_at")
	}
	if minted.ExpiresAt.Location() != time.UTC {
		t.Errorf("the record expires_at %s is not in UTC", minted.ExpiresAt)
	}

	var c claims
	if _, err := jwt.ParseWithClaims(minted.Token, &c, func(*jwt.Token) (any, error) { return []byte(testSecret), nil },
		jwt.WithValidMethods([]string{"HS256"})); err != nil {
		t.Fatalf("parse the minted token: %v", err)
	}
	if minted.ExpiresAt.Unix() != c.ExpiresAt.Unix() {
		t.Errorf("the record expires_at %d does not equal the token exp %d", minted.ExpiresAt.Unix(), c.ExpiresAt.Unix())
	}
}

// With no scopes MintToken defaults to ["*"] and writes it into the token, so the record and token agree.
func TestMintTokenDefaultsAndWritesTheScopesClaim(t *testing.T) {
	minted, err := MintToken([]byte(testSecret), "ci", nil, time.Hour)
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	if strings.Join(minted.Scopes, ",") != "*" {
		t.Errorf("the record carries scopes %v, want [\"*\"] by default", minted.Scopes)
	}

	verified, err := verify([]byte(testSecret), minted.Token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if strings.Join(verified.Scopes, ",") != "*" {
		t.Errorf("the token carries scopes %v, want the written default [\"*\"]", verified.Scopes)
	}
}

func TestSetConnectionCloseForcesConnectionClose(t *testing.T) {
	head := []byte("GET /v0/sandboxes HTTP/1.1\r\nHost: box\r\nConnection: keep-alive\r\n\r\n")

	got := string(setConnectionClose(head))
	if strings.Count(strings.ToLower(got), "connection:") != 1 {
		t.Errorf("the forwarded head is %q, want exactly one Connection header", got)
	}
	if !strings.Contains(got, "Connection: close\r\n") {
		t.Errorf("the forwarded head is %q, want Connection: close", got)
	}
	if strings.Contains(strings.ToLower(got), "keep-alive") {
		t.Errorf("the forwarded head is %q, want the client's Connection dropped", got)
	}
	if !strings.HasPrefix(got, "GET /v0/sandboxes HTTP/1.1\r\n") {
		t.Errorf("the forwarded head lost the request line: %q", got)
	}
	if !strings.HasSuffix(got, "\r\n\r\n") {
		t.Errorf("the forwarded head does not end the headers: %q", got)
	}
}

// The front stamps the token's scopes and drops any the client forged, so the daemon trusts only the front's copy.
func TestStampScopesReplacesAForgedClientHeader(t *testing.T) {
	head := []byte("POST /v0/sandboxes HTTP/1.1\r\nHost: box\r\nX-Shard-Scopes: secret:*,policy:*\r\n\r\n")

	got := string(stampScopes(head, []string{"sandbox:write", "exec"}))
	if strings.Count(got, "X-Shard-Scopes:") != 1 {
		t.Errorf("the forwarded head is %q, want exactly one X-Shard-Scopes header", got)
	}
	if !strings.Contains(got, "X-Shard-Scopes: sandbox:write,exec\r\n") {
		t.Errorf("the forwarded head is %q, want the token's scopes stamped", got)
	}
	if strings.Contains(got, "secret:*") || strings.Contains(got, "policy:*") {
		t.Errorf("the forwarded head is %q, want the client's forged scopes dropped", got)
	}
	if !strings.HasPrefix(got, "POST /v0/sandboxes HTTP/1.1\r\n") {
		t.Errorf("the forwarded head lost the request line: %q", got)
	}
	if !strings.HasSuffix(got, "\r\n\r\n") {
		t.Errorf("the forwarded head does not end the headers: %q", got)
	}
}

// A lone Upgrade header is not a handshake: without all four headers the front must force Connection: close.
func TestIsHandshakeNeedsAllFourHeaders(t *testing.T) {
	full := "GET /v0/sandboxes/s1/logs HTTP/1.1\r\nHost: box\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"
	if !isHandshake([]byte(full)) {
		t.Error("a full four-header handshake was not recognized")
	}

	for name, head := range map[string]string{
		"no key":        "GET /v0/sandboxes/s1/logs HTTP/1.1\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\n\r\n",
		"no version":    "GET /v0/sandboxes/s1/logs HTTP/1.1\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n",
		"no upgrade":    "GET /v0/sandboxes/s1/logs HTTP/1.1\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n",
		"no connection": "GET /v0/sandboxes/s1/logs HTTP/1.1\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n",
		"wrong version": "GET /v0/sandboxes/s1/logs HTTP/1.1\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 8\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n",
		"plain request": "GET /v0/sandboxes HTTP/1.1\r\nHost: box\r\n\r\n",
	} {
		if isHandshake([]byte(head)) {
			t.Errorf("%s: a partial upgrade was treated as a handshake", name)
		}
	}
}

// Every way a proxied connection ends is quiet as net wraps it, on either platform, and any other failure is not.
func TestQuietReadsEveryEndOfAProxiedConnection(t *testing.T) {
	wrap := func(errno syscall.Errno) error {
		return &net.OpError{Op: "write", Net: "unix", Err: os.NewSyscallError("write", errno)}
	}
	for name, err := range map[string]error{
		"nil":           nil,
		"eof":           io.EOF,
		"closed":        net.ErrClosed,
		"deadline":      os.ErrDeadlineExceeded,
		"broken pipe":   wrap(syscall.EPIPE),
		"reset":         wrap(syscall.ECONNRESET),
		"not connected": wrap(syscall.ENOTCONN),
		"wrapped":       fmt.Errorf("write stream output: %w", wrap(syscall.ENOTCONN)),
	} {
		if !quiet(err) {
			t.Errorf("%s: %v reads as a failure, want quiet", name, err)
		}
	}
	for name, err := range map[string]error{
		"permission": wrap(syscall.EACCES),
		"refused":    wrap(syscall.ECONNREFUSED),
		"other":      errors.New("read the response status line: boom"),
	} {
		if quiet(err) {
			t.Errorf("%s: %v reads as quiet, want a failure", name, err)
		}
	}
}

// A sandbox:read token lists and inspects, and is 403 on every route it does not name; a forbidden route is never dialed.
func TestAScopedTokenReachesOnlyItsRoutes(t *testing.T) {
	up := fakeDaemon(t)
	env := newTokenEnv(t)
	address := front(t, up.root, env.secret)
	token := mintScoped(t, env, "reader", "sandbox:read")

	if resp := askRoute(t, address, token, http.MethodGet, "/v0/sandboxes"); resp.StatusCode != http.StatusOK { //nolint:bodyclose // askRoute closes the body in a cleanup
		t.Errorf("a sandbox:read token got %d on a read, want 200", resp.StatusCode)
	}

	denied := []struct{ method, path string }{
		{http.MethodPost, "/v0/sandboxes"},
		{http.MethodDelete, "/v0/sandboxes/s1"},
		{http.MethodPost, "/v0/sandboxes/s1/exec"},
		{http.MethodGet, "/v0/secrets"},
	}
	for _, d := range denied {
		resp := askRoute(t, address, token, d.method, d.path) //nolint:bodyclose // askRoute closes the body in a cleanup
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("a sandbox:read token got %d on %s %s, want 403", resp.StatusCode, d.method, d.path)
		}

		var body struct {
			Error struct {
				Code    models.Code `json:"code"`
				Message string      `json:"message"`
			} `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("%s %s: decode the refusal: %v", d.method, d.path, err)
		}
		if body.Error.Code != models.CodeForbidden || body.Error.Message == "" {
			t.Errorf("%s %s: the refusal reads %+v, want a line and the code forbidden", d.method, d.path, body)
		}
	}

	// The one read dialed the socket; every forbidden route was answered without a dial.
	if dialed := up.dialed.Load(); dialed != 1 {
		t.Errorf("the front dialed the socket %d times, want 1 for the single read it allowed", dialed)
	}
}

// A snapshot copies a sandbox's files, so making one takes sandbox:write and removing one sandbox:delete.
func TestASandboxReadTokenListsSnapshotsAndNeitherMakesNorRemovesOne(t *testing.T) {
	up := fakeDaemon(t)
	env := newTokenEnv(t)
	address := front(t, up.root, env.secret)
	token := mintScoped(t, env, "reader", "sandbox:read")

	for _, path := range []string{"/v0/snapshots", "/v0/snapshots/base"} {
		if resp := askRoute(t, address, token, http.MethodGet, path); resp.StatusCode != http.StatusOK { //nolint:bodyclose // askRoute closes the body in a cleanup
			t.Errorf("a sandbox:read token got %d on GET %s, want 200", resp.StatusCode, path)
		}
	}
	for _, d := range []struct{ method, path string }{{http.MethodPost, "/v0/snapshots"}, {http.MethodDelete, "/v0/snapshots/base"}} {
		if resp := askRoute(t, address, token, d.method, d.path); resp.StatusCode != http.StatusForbidden { //nolint:bodyclose // askRoute closes the body in a cleanup
			t.Errorf("a sandbox:read token got %d on %s %s, want 403", resp.StatusCode, d.method, d.path)
		}
	}
}

// A token with no scopes and a token with a "*" scope both reach a write route.
func TestAFullTokenReachesAWriteRoute(t *testing.T) {
	up := fakeDaemon(t)
	env := newTokenEnv(t)
	address := front(t, up.root, env.secret)

	for name, token := range map[string]string{
		"no scopes": mint(t, env, "root"),
		"star":      mintScoped(t, env, "root", "*"),
	} {
		if resp := askRoute(t, address, token, http.MethodPost, "/v0/sandboxes"); resp.StatusCode != http.StatusOK { //nolint:bodyclose // askRoute closes the body in a cleanup
			t.Errorf("%s: a full token got %d on a write, want 200", name, resp.StatusCode)
		}
	}
}

// An unknown route is 403 for any token, and the front never dials the daemon for it.
func TestAnUnknownRouteIs403AndNothingIsDialed(t *testing.T) {
	up := fakeDaemon(t)
	env := newTokenEnv(t)
	address := front(t, up.root, env.secret)

	resp := askRoute(t, address, mint(t, env, "root"), http.MethodGet, "/v0/nonesuch") //nolint:bodyclose // askRoute closes the body in a cleanup
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("an unknown route got %d, want 403", resp.StatusCode)
	}
	if dialed := up.dialed.Load(); dialed != 0 {
		t.Errorf("the front dialed the socket %d times for an unknown route, want none", dialed)
	}
}

// A local route reads like an unknown one to every token: the same 403 body, and the front never dials the daemon for it.
func TestALocalRouteIs403ForEveryTokenAndNothingIsDialed(t *testing.T) {
	up := fakeDaemon(t)
	env := newTokenEnv(t)
	address := front(t, up.root, env.secret)

	tokens := map[string]string{"no scopes": mint(t, env, "root")}
	for _, scope := range models.Scopes {
		tokens[scope.Name] = mintScoped(t, env, "scoped", scope.Name)
	}

	var locals []api.Route
	for _, r := range api.Routes() {
		if r.Class == api.Local {
			locals = append(locals, r)
		}
	}
	if len(locals) < 5 {
		t.Fatalf("the walk found %d local routes, want the daemon status and the four image routes", len(locals))
	}

	for name, token := range tokens {
		checkLocalRoutesRefused(t, address, name, token, locals)
	}

	if dialed := up.dialed.Load(); dialed != 0 {
		t.Errorf("the front dialed the socket %d times for a local route, want none", dialed)
	}
}

// checkLocalRoutesRefused fails unless every local route answers one token the 403 and the body an unknown route gets.
func checkLocalRoutesRefused(t *testing.T, address, name, token string, locals []api.Route) {
	t.Helper()

	unknown := readAll(t, askRoute(t, address, token, http.MethodGet, "/v0/nonesuch")) //nolint:bodyclose // askRoute closes the body in a cleanup
	if unknown != unrouted+"\n" {
		t.Fatalf("%s: an unknown route answered %q, want the unrouted body", name, unknown)
	}
	for _, r := range locals {
		path := strings.NewReplacer("{id}", "s1", "{ref...}", "alpine").Replace(r.Pattern)
		resp := askRoute(t, address, token, r.Method, path) //nolint:bodyclose // askRoute closes the body in a cleanup
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: %s %s got %d, want 403", name, r.Method, path, resp.StatusCode)
		}
		if body := readAll(t, resp); body != unknown {
			t.Errorf("%s: %s %s answered %q, want the unknown-route body %q", name, r.Method, path, body, unknown)
		}
	}
}

// The SDK handshake and scope discovery work for every token, so they need no scope a token could lack.
func TestDiscoveryAnswersAnyValidToken(t *testing.T) {
	up := fakeDaemon(t)
	env := newTokenEnv(t)
	address := front(t, up.root, env.secret)
	token := mintScoped(t, env, "secrets", "secret:*")

	for _, path := range []string{"/v0/version", "/v0/capabilities", "/v0/scopes"} {
		if resp := askRoute(t, address, token, http.MethodGet, path); resp.StatusCode != http.StatusOK { //nolint:bodyclose // askRoute closes the body in a cleanup
			t.Errorf("a secret:* token got %d on GET %s, want 200", resp.StatusCode, path)
		}
		if resp := askRoute(t, address, "", http.MethodGet, path); resp.StatusCode != http.StatusUnauthorized { //nolint:bodyclose // askRoute closes the body in a cleanup
			t.Errorf("no token got %d on GET %s, want 401", resp.StatusCode, path)
		}
	}
}

// No public route needs daemon:read or image:*, so a mint naming one would hand out a scope that opens nothing.
func TestCheckScopesRefusesTheRetiredScopes(t *testing.T) {
	for _, scope := range []string{"daemon:read", "image:*"} {
		if err := CheckScopes([]string{scope}); err == nil {
			t.Errorf("CheckScopes took %q, want a refusal", scope)
		}
	}
}

// countingProcess answers GET /v0/daemon and counts each call, so a test sees whether that local handler ran.
type countingProcess struct {
	calls *atomic.Int64
}

func (p countingProcess) Daemon() (api.Daemon, error) {
	p.calls.Add(1)

	return api.Daemon{Provider: "gvisor"}, nil
}

// liveDaemon is the daemon's own api mux on a socket under root, and what a test reads of it.
type liveDaemon struct {
	root       string
	process    countingProcess
	dispatched chan string
	// hungUp closes once the daemon has closed a connection, after which that connection dispatches nothing more.
	hungUp chan struct{}
}

// realDaemon serves the daemon's own api mux on a socket under a root, and records the path of each request the mux dispatched.
func realDaemon(t *testing.T) liveDaemon {
	t.Helper()

	root := shortRoot(t)
	listener, err := net.Listen("unix", filepath.Join(root, "shard.sock"))
	if err != nil {
		t.Fatalf("listen on the socket: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	d := liveDaemon{root: root, process: countingProcess{calls: &atomic.Int64{}}, dispatched: make(chan string, 8), hungUp: make(chan struct{})}
	hangUp := sync.OnceFunc(func() { close(d.hungUp) })
	mux := api.NewHandler("v-test", d.process, nil, nil, nil, nil, nil, nil, io.Discard)
	server := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			d.dispatched <- r.Method + " " + r.URL.Path
			mux.ServeHTTP(w, r)
		}),
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateClosed {
				hangUp()
			}
		},
	}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })

	return d
}

// noticeListener hands out connections that call notice once the front has closed one.
type noticeListener struct {
	net.Listener

	notice func()
}

func (l noticeListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return nil, fmt.Errorf("the front accepted a %T, want a TCP connection", conn)
	}

	return noticedConn{TCPConn: tcp, notice: l.notice}, nil
}

// noticedConn keeps the CloseWrite the front half-closes with, and calls notice after Close returns.
type noticedConn struct {
	*net.TCPConn

	notice func()
}

func (c noticedConn) Close() error {
	defer c.notice()

	return c.TCPConn.Close()
}

// await fails t unless done closes within the bound every read of these tests has.
func await(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s within 5s", what)
	}
}

// A local route pipelined behind a public request is never dispatched: the front forwards the bytes, and the daemon answers one request per connection.
func TestAPipelinedLocalRouteIsNeverDispatched(t *testing.T) {
	d := realDaemon(t)
	env := newTokenEnv(t)
	frontClosed := make(chan struct{})
	address := frontWith(t, d.root, env.secret, func(_ *Server, listener net.Listener) net.Listener {
		return noticeListener{Listener: listener, notice: sync.OnceFunc(func() { close(frontClosed) })}
	})
	token := mintScoped(t, env, "root", "*")

	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("dial the front: %v", err)
	}
	defer conn.Close()
	// A daemon that keeps the connection open never sends EOF, so the read fails here instead of hanging the run.
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set the deadline: %v", err)
	}

	public := "GET /v0/version HTTP/1.1\r\nHost: box\r\nAuthorization: Bearer " + token + "\r\n\r\n"
	local := "GET /v0/daemon HTTP/1.1\r\nHost: box\r\nAuthorization: Bearer " + token + "\r\n\r\n"
	if _, err := io.WriteString(conn, public+local); err != nil {
		t.Fatalf("write the pipelined requests: %v", err)
	}

	// A close over the unread pipelined request is a reset, so the read starts only after the front's close, where one would have destroyed the answer.
	await(t, frontClosed, "the front did not close the connection")

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("read the first answer: %v", err)
	}
	defer resp.Body.Close()
	if body := readAll(t, resp); resp.StatusCode != http.StatusOK || !strings.Contains(body, `"api_version"`) {
		t.Errorf("the public request got %d %q, want the version", resp.StatusCode, body)
	}
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read past the first answer: %v", err)
	}
	if len(rest) != 0 {
		t.Errorf("the front sent %q after the first answer, want EOF", rest)
	}

	await(t, d.hungUp, "the daemon did not close its connection")
	got := make([]string, 0, len(d.dispatched))
	for range len(d.dispatched) {
		got = append(got, <-d.dispatched)
	}
	if !slices.Equal(got, []string{"GET /v0/version"}) {
		t.Errorf("the daemon dispatched %v, want only the public request", got)
	}
	if calls := d.process.calls.Load(); calls != 0 {
		t.Errorf("the local GET /v0/daemon handler ran %d times, want none", calls)
	}
}

// readAll answers the body of resp as a string.
func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the body: %v", err)
	}

	return string(body)
}

// A public route with no scope a token can carry would be reachable by a "*" token alone, and a local route with one reads as forwardable.
func TestEveryPublicRouteAndNoOtherNeedsAScopeATokenCanCarry(t *testing.T) {
	covered := 0
	for _, r := range api.Routes() {
		covered++
		if r.Class == api.Public && r.Scope != api.AnyToken && (r.Scope == models.ScopeAll || CheckScopes([]string{string(r.Scope)}) != nil) {
			t.Errorf("public route %s %s needs %q, which mint refuses as a scope", r.Method, r.Pattern, r.Scope)
		}
		if r.Class == api.Local && r.Scope != "" {
			t.Errorf("local route %s %s needs %q, but the front never forwards it", r.Method, r.Pattern, r.Scope)
		}
	}

	// An empty route list would pass in silence, so the walk proves it covered the daemon surface.
	if covered < 15 {
		t.Fatalf("the walk covered %d daemon routes, want the full set", covered)
	}
}

// A non-101 answer to a handshake ends after one response, so a pipelined second request never reaches the daemon.
func TestANonUpgradeAnswerDoesNotForwardAPipelinedRequest(t *testing.T) {
	up := plainDaemon(t)
	env := newTokenEnv(t)
	address := front(t, up.root, env.secret)
	token := mint(t, env, "root")

	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("dial the front: %v", err)
	}
	defer conn.Close()

	handshake := "GET /v0/sandboxes/s1/logs HTTP/1.1\r\nHost: box\r\nAuthorization: Bearer " + token +
		"\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"
	pipelined := "GET /v0/sandboxes HTTP/1.1\r\nHost: box\r\nAuthorization: Bearer " + token + "\r\n\r\n"
	if _, err := io.WriteString(conn, handshake+pipelined); err != nil {
		t.Fatalf("write the pipelined requests: %v", err)
	}

	// A front that closes with the pipelined request unread sends a reset, and by now it would have destroyed the answer.
	time.Sleep(200 * time.Millisecond)

	// The front relays the daemon's one non-101 response and closes; ReadAll ends when it does.
	if _, err := io.ReadAll(conn); err != nil {
		t.Fatalf("read the front's answer: %v", err)
	}

	if got := drain(up.requests); len(got) != 1 {
		t.Errorf("the daemon saw %d requests, want only the handshake, never the pipelined one: %v", len(got), got)
	}
}

// The front's own answer survives a request it never reads, so a client with a body and no token still reads its 401.
func TestTheFrontsOwnAnswerSurvivesAnUnreadRequest(t *testing.T) {
	up := fakeDaemon(t)
	env := newTokenEnv(t)
	address := front(t, up.root, env.secret)

	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("dial the front: %v", err)
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, "POST /v0/sandboxes HTTP/1.1\r\nHost: box\r\nContent-Length: 2\r\n\r\n{}"); err != nil {
		t.Fatalf("write the request: %v", err)
	}

	// A front that closes with the body unread sends a reset, and by now it would have destroyed the answer.
	time.Sleep(200 * time.Millisecond)

	answer, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read the front's answer: %v", err)
	}
	if !strings.HasPrefix(string(answer), "HTTP/1.1 401 ") {
		t.Errorf("the front answered %q, want its 401", answer)
	}
}

// A bare LF ends the head for the daemon but not for the front's scan, so a request framed that way hides a second request the scope check never sees. The front must refuse it and dial nothing.
func TestABareLFFramedRequestIsRefusedAndNothingReachesTheDaemon(t *testing.T) {
	up := fakeDaemon(t)
	env := newTokenEnv(t)
	address := front(t, up.root, env.secret)
	token := mintScoped(t, env, "reader", "sandbox:read")

	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("dial the front: %v", err)
	}
	defer conn.Close()

	// The read is framed with bare LFs; the smuggled delete is a route the reader token has no scope for.
	smuggle := "GET /v0/sandboxes HTTP/1.1\nHost: box\nAuthorization: Bearer " + token + "\n\n" +
		"DELETE /v0/sandboxes/s1 HTTP/1.1\r\nHost: box\r\nAuthorization: Bearer " + token + "\r\n\r\n"
	if _, err := io.WriteString(conn, smuggle); err != nil {
		t.Fatalf("write the smuggled requests: %v", err)
	}

	if got := drain(up.requests); len(got) != 0 {
		t.Errorf("the daemon saw %v, want nothing: the front must refuse the bare-LF head before it dials", got)
	}
	if dialed := up.dialed.Load(); dialed != 0 {
		t.Errorf("the front dialed the socket %d times for a bare-LF head, want none", dialed)
	}
}

func TestReadHeadRefusesABareLineFeed(t *testing.T) {
	if _, err := readHead(strings.NewReader("GET / HTTP/1.1\nHost: box\n\n")); err == nil {
		t.Fatal("readHead accepted a head framed with bare line feeds")
	}

	strict := "GET / HTTP/1.1\r\nHost: box\r\n\r\n"
	head, err := readHead(strings.NewReader(strict))
	if err != nil {
		t.Fatalf("readHead refused a strict CRLF head: %v", err)
	}
	if string(head) != strict {
		t.Errorf("readHead returned %q, want the strict head %q", head, strict)
	}
}

// A token minted with no duration carries no exp, still verifies, and lists as active.
func TestMintWithNoDurationHasNoExpiry(t *testing.T) {
	env := newTokenEnv(t)

	minted, err := IssueToken([]byte(testSecret), env.tokens, "ci", nil, 0)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if minted.ExpiresAt != nil {
		t.Errorf("the record carries expires_at %s, want none for a token that never expires", minted.ExpiresAt)
	}

	if _, err := verify([]byte(testSecret), minted.Token); err != nil {
		t.Fatalf("verify a token with no expiry: %v", err)
	}

	infos, err := ListTokens(env.tokens)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(infos) != 1 || infos[0].Status != StatusActive || infos[0].ExpiresAt != nil {
		t.Errorf("the ledger lists %+v, want one active token with no expiry", infos)
	}
}

// A token whose signature is valid but whose id the ledger does not hold is 401, and nothing is dialed.
func TestATokenNotInTheLedgerIs401(t *testing.T) {
	up := fakeDaemon(t)
	env := newTokenEnv(t)
	address := front(t, up.root, env.secret)

	// Mint signs without recording, so the token is well-formed but absent from the front's ledger.
	token, err := Mint([]byte(testSecret), "ghost", nil, time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	resp := ask(t, address, token) //nolint:bodyclose // ask closes the body in a cleanup
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a token not in the ledger got %d, want 401", resp.StatusCode)
	}
	if dialed := up.dialed.Load(); dialed != 0 {
		t.Errorf("the front dialed the socket %d times for a token not in the ledger, want none", dialed)
	}
}

// A token works, then a revoke marks it in the ledger, and the very next request with it is 401, no restart.
func TestARevokedTokenIs401OnTheNextRequest(t *testing.T) {
	up := fakeDaemon(t)
	env := newTokenEnv(t)
	address := front(t, up.root, env.secret)
	token := mint(t, env, "ci")

	if resp := ask(t, address, token); resp.StatusCode != http.StatusOK { //nolint:bodyclose // ask closes the body in a cleanup
		t.Fatalf("the token got %d before the revoke, want 200", resp.StatusCode)
	}

	found, err := RevokeSubject(env.tokens, "ci")
	if err != nil {
		t.Fatalf("RevokeSubject: %v", err)
	}
	if found != 1 {
		t.Fatalf("revoke matched %d tokens, want 1", found)
	}

	if resp := ask(t, address, token); resp.StatusCode != http.StatusUnauthorized { //nolint:bodyclose // ask closes the body in a cleanup
		t.Errorf("the token got %d after the revoke, want 401", resp.StatusCode)
	}
}

// A ledger that was there and then vanishes refuses every request; the front never downgrades to no ledger.
func TestAVanishedLedgerRefusesEveryRequest(t *testing.T) {
	up := fakeDaemon(t)
	env := newTokenEnv(t)
	address := front(t, up.root, env.secret)
	token := mint(t, env, "ci")

	if resp := ask(t, address, token); resp.StatusCode != http.StatusOK { //nolint:bodyclose // ask closes the body in a cleanup
		t.Fatalf("the token got %d before the ledger vanished, want 200", resp.StatusCode)
	}

	if err := os.Remove(env.tokens); err != nil {
		t.Fatalf("remove the ledger: %v", err)
	}

	if resp := ask(t, address, token); resp.StatusCode != http.StatusUnauthorized { //nolint:bodyclose // ask closes the body in a cleanup
		t.Errorf("the token got %d after the ledger vanished, want 401", resp.StatusCode)
	}
}

// The front refuses to start when a ledger others can read is there, because it names every valid token.
func TestTheFrontRefusesAWorldReadableLedger(t *testing.T) {
	env := newTokenEnv(t)
	if _, err := IssueToken([]byte(testSecret), env.tokens, "ci", nil, time.Hour); err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if err := os.Chmod(env.tokens, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	_, err := New(Config{Listen: "127.0.0.1:0", SigningKeyFile: env.secret, Root: shortRoot(t), Out: io.Discard})
	if err == nil {
		t.Error("the front started with a world-readable ledger, want a refusal")
	}
}

// ListTokens reads active, revoked and expired straight from the ledger records.
func TestListTokensReadsEachStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), TokensFileName)

	now := time.Now().UTC().Truncate(time.Second)
	past := now.Add(-time.Hour)
	entries := []ledgerEntry{
		{JTI: "active-id", Sub: "a", IssuedAt: now, Scopes: []string{"*"}},
		{JTI: "revoked-id", Sub: "b", IssuedAt: now, Scopes: []string{"*"}, Revoked: true},
		{JTI: "expired-id", Sub: "c", IssuedAt: past, ExpiresAt: &past, Scopes: []string{"*"}},
	}
	for _, e := range entries {
		if err := appendEntry(path, e); err != nil {
			t.Fatalf("appendEntry: %v", err)
		}
	}

	infos, err := ListTokens(path)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}

	want := map[string]TokenStatus{"active-id": StatusActive, "revoked-id": StatusRevoked, "expired-id": StatusExpired}
	if len(infos) != len(want) {
		t.Fatalf("ListTokens read %d tokens, want %d", len(infos), len(want))
	}
	for _, info := range infos {
		if info.Status != want[info.ID] {
			t.Errorf("token %s lists as %s, want %s", info.ID, info.Status, want[info.ID])
		}
	}
}

// Revoke by id flips one token; revoke by subject flips every token that subject holds; an unknown id matches none.
func TestRevokeMarksTokensInTheLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), TokensFileName)

	for _, sub := range []string{"alice", "alice", "bob"} {
		if _, err := IssueToken([]byte(testSecret), path, sub, nil, time.Hour); err != nil {
			t.Fatalf("IssueToken: %v", err)
		}
	}

	infos, err := ListTokens(path)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	var bob string
	for _, info := range infos {
		if info.Subject == "bob" {
			bob = info.ID
		}
	}

	if found, err := RevokeToken(path, bob); err != nil || found != 1 {
		t.Fatalf("RevokeToken matched %d (err %v), want 1", found, err)
	}
	if found, err := RevokeSubject(path, "alice"); err != nil || found != 2 {
		t.Fatalf("RevokeSubject matched %d (err %v), want 2", found, err)
	}

	infos, err = ListTokens(path)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	for _, info := range infos {
		if info.Status != StatusRevoked {
			t.Errorf("token %s of %s lists as %s, want revoked", info.ID, info.Subject, info.Status)
		}
	}

	if found, err := RevokeToken(path, "no-such-id"); err != nil || found != 0 {
		t.Errorf("RevokeToken on an unknown id matched %d (err %v), want 0 and no error", found, err)
	}
}

// Two concurrent revokes of different tokens both persist; without the ledger lock the later rewrite drops the earlier flip.
func TestConcurrentRevokesBothPersist(t *testing.T) {
	for iteration := range 50 {
		path := filepath.Join(t.TempDir(), TokensFileName)
		for _, sub := range []string{"alice", "bob"} {
			if _, err := IssueToken([]byte(testSecret), path, sub, nil, time.Hour); err != nil {
				t.Fatalf("IssueToken: %v", err)
			}
		}

		infos, err := ListTokens(path)
		if err != nil {
			t.Fatalf("ListTokens: %v", err)
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, info := range infos {
			id := info.ID
			wg.Go(func() {
				<-start
				if _, err := RevokeToken(path, id); err != nil {
					t.Errorf("RevokeToken: %v", err)
				}
			})
		}
		close(start)
		wg.Wait()

		after, err := ListTokens(path)
		if err != nil {
			t.Fatalf("ListTokens after the revokes: %v", err)
		}
		for _, info := range after {
			if info.Status != StatusRevoked {
				t.Fatalf("iteration %d: token %s of %s lists as %s, want revoked", iteration, info.ID, info.Subject, info.Status)
			}
		}
	}
}

// A mint concurrent with a revoke keeps both records; without the ledger lock the revoke's rewrite drops the appended token.
func TestConcurrentMintAndRevokeLoseNoRecord(t *testing.T) {
	for iteration := range 50 {
		path := filepath.Join(t.TempDir(), TokensFileName)
		if _, err := IssueToken([]byte(testSecret), path, "alice", nil, time.Hour); err != nil {
			t.Fatalf("IssueToken: %v", err)
		}

		infos, err := ListTokens(path)
		if err != nil {
			t.Fatalf("ListTokens: %v", err)
		}
		alice := infos[0].ID

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() {
			<-start
			if _, err := RevokeToken(path, alice); err != nil {
				t.Errorf("RevokeToken: %v", err)
			}
		})
		wg.Go(func() {
			<-start
			if _, err := IssueToken([]byte(testSecret), path, "bob", nil, time.Hour); err != nil {
				t.Errorf("IssueToken: %v", err)
			}
		})
		close(start)
		wg.Wait()

		after, err := ListTokens(path)
		if err != nil {
			t.Fatalf("ListTokens after the mint and revoke: %v", err)
		}
		if len(after) != 2 {
			t.Fatalf("iteration %d: the ledger holds %d tokens, want 2; the mint was lost", iteration, len(after))
		}
	}
}

// IssueToken creates the ledger at 0640 when it is absent, so the group reads it and others do not.
func TestIssueTokenCreatesTheLedger0640(t *testing.T) {
	path := filepath.Join(t.TempDir(), TokensFileName)

	if _, err := IssueToken([]byte(testSecret), path, "ci", nil, time.Hour); err != nil {
		t.Fatalf("IssueToken: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the ledger: %v", err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("the ledger is at mode %04o, want 0640", info.Mode().Perm())
	}
}

// IssueToken refuses to append to a ledger others can read, so a minted token never lands in an exposed file.
func TestIssueTokenRefusesAWorldReadableLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), TokensFileName)

	if _, err := IssueToken([]byte(testSecret), path, "ci", nil, time.Hour); err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if _, err := IssueToken([]byte(testSecret), path, "ci", nil, time.Hour); err == nil {
		t.Error("IssueToken appended to a world-readable ledger, want a refusal")
	}
}

// tokenEnv is a secret file and the ledger beside it, so a token minted here lands where the front reads it.
type tokenEnv struct {
	secret string
	tokens string
}

func newTokenEnv(t *testing.T) tokenEnv {
	t.Helper()

	secret := secretFile(t, testSecret)

	return tokenEnv{secret: secret, tokens: TokensPath(secret)}
}

// mint issues a valid token for sub over the test secret, records it in env's ledger, and answers it.
func mint(t *testing.T, env tokenEnv, sub string) string {
	t.Helper()

	return mintScoped(t, env, sub)
}

// mintScoped issues a valid token for sub over the test secret, recording it in env's ledger with the scopes named.
func mintScoped(t *testing.T, env tokenEnv, sub string, scopes ...string) string {
	t.Helper()

	minted, err := IssueToken([]byte(testSecret), env.tokens, sub, scopes, time.Hour)
	if err != nil {
		t.Fatalf("mint a token: %v", err)
	}

	return minted.Token
}

// signed builds a token of method over key, for the refusal cases the front must reject.
func signed(t *testing.T, method jwt.SigningMethod, key any, claims jwt.Claims) string {
	t.Helper()

	token, err := jwt.NewWithClaims(method, claims).SignedString(key)
	if err != nil {
		t.Fatalf("sign a test token: %v", err)
	}

	return token
}

func secretFile(t *testing.T, value string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatalf("write the secret file: %v", err)
	}

	return path
}

// shortRoot keeps a unix socket path under the 104 bytes macOS allows.
func shortRoot(t *testing.T) string {
	t.Helper()

	root, err := os.MkdirTemp("", "shard") //nolint:usetesting // t.TempDir is too long for a socket path
	if err != nil {
		t.Fatalf("make a root: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	return root
}

// A listed scope that no public route needs would be an admin scope a remote token could carry and never use.
func TestEveryListedScopeOpensAPublicRoute(t *testing.T) {
	for _, scope := range models.Scopes {
		if scope.Name == models.ScopeAll {
			continue
		}
		opens := false
		for _, r := range api.Routes() {
			opens = opens || (r.Class == api.Public && string(r.Scope) == scope.Name)
		}
		if !opens {
			t.Errorf("scope %s opens no public route", scope.Name)
		}
	}
}
