package serve

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type guardStream struct {
	upgrade bool
	active  bool
	delayed bool
}

func openGuardStream(t *testing.T, env tokenEnv, ttl time.Duration, mode guardStream) (io.Reader, <-chan struct{}) {
	t.Helper()
	root := shortRoot(t)
	listener, err := net.Listen("unix", filepath.Join(root, "shard.sock"))
	if err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		conn, err := listener.Accept()
		if err != nil {
			t.Errorf("accept the stream: %v", err)

			return
		}
		defer func() {
			if err := conn.Close(); !quiet(err) {
				t.Errorf("close the stream: %v", err)
			}
		}()
		reader := bufio.NewReader(conn)
		req, err := http.ReadRequest(reader)
		if err != nil {
			t.Errorf("read the stream request: %v", err)

			return
		}
		if err := req.Body.Close(); err != nil {
			t.Errorf("close the request body: %v", err)

			return
		}
		hungUp := make(chan struct{})
		go func() {
			if _, err := io.Copy(io.Discard, reader); !quiet(err) {
				t.Errorf("read the stream: %v", err)
			}
			close(hungUp)
		}()
		if mode.delayed {
			<-hungUp

			return
		}
		head := "HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n"
		if mode.upgrade {
			head = "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n\r\n"
		}
		if _, err := io.WriteString(conn, head); err != nil {
			t.Errorf("write the stream response: %v", err)

			return
		}
		if !mode.active {
			<-hungUp

			return
		}
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-hungUp:
				return
			case <-ticker.C:
				if _, err := io.WriteString(conn, "\x81\x01x"); err != nil {
					if !quiet(err) {
						t.Errorf("write stream output: %v", err)
					}
					<-hungUp

					return
				}
			}
		}
	}()
	t.Cleanup(func() {
		if err := listener.Close(); !quiet(err) {
			t.Error(err)
		}
		await(t, ended, "the stream peer did not close")
	})
	minted, err := IssueToken([]byte(testSecret), env.tokens, "ci", nil, ttl)
	if err != nil {
		t.Fatal(err)
	}
	address := front(t, root, env.secret)
	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(); !quiet(err) {
			t.Error(err)
		}
	})
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	upgrade := ""
	if mode.upgrade {
		upgrade = "Connection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"
	}
	if _, err := fmt.Fprintf(client, "GET /v0/sandboxes/test/logs?follow=true HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer %s\r\n%s\r\n", minted.Token, upgrade); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(client)
	var output io.Reader = reader
	if !mode.delayed {
		response, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := response.Body.Close(); err != nil {
				t.Error(err)
			}
		})
		if !mode.upgrade {
			output = response.Body
		}
		want := http.StatusOK
		if mode.upgrade {
			want = http.StatusSwitchingProtocols
		}
		if response.StatusCode != want {
			t.Fatalf("the stream answered %d, want %d", response.StatusCode, want)
		}
	}

	return output, ended
}

func TestTokenExpiryEndsSilentActiveAndDelayedStreams(t *testing.T) {
	for name, mode := range map[string]guardStream{
		"silent websocket":  {upgrade: true},
		"active websocket":  {upgrade: true, active: true},
		"plain HTTP follow": {active: true},
		"delayed handshake": {upgrade: true, delayed: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newTokenEnv(t)
			reader, ended := openGuardStream(t, env, 2*time.Second, mode)
			body, err := io.ReadAll(reader)
			if err != nil {
				t.Fatalf("the expired stream stayed open: %v", err)
			}
			if mode.active && len(body) == 0 {
				t.Fatal("the active stream sent no output before expiry")
			}
			await(t, ended, "expiry did not close the daemon peer")
		})
	}
}

func TestLedgerChangesEndAnActiveConnection(t *testing.T) {
	for _, change := range []string{"revoke", "no-exp revoke", "absent id", "malformed ledger", "missing ledger", "unreadable ledger"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			if change == "unreadable ledger" && os.Geteuid() == 0 {
				t.Skip("root can read a ledger with no mode bits")
			}
			env := newTokenEnv(t)
			ttl := time.Hour
			if change == "no-exp revoke" {
				ttl = 0
			}
			reader, ended := openGuardStream(t, env, ttl, guardStream{upgrade: change != "revoke", active: change == "revoke"})
			var err error
			switch change {
			case "revoke", "no-exp revoke":
				_, err = RevokeSubject(env.tokens, "ci")
			case "absent id":
				err = writeEntries(env.tokens, nil)
			case "malformed ledger":
				err = os.WriteFile(env.tokens, []byte("{\n"), 0o640)
			case "missing ledger":
				err = os.Remove(env.tokens)
			case "unreadable ledger":
				err = os.Chmod(env.tokens, 0)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadAll(reader); err != nil {
				t.Fatalf("the refused stream stayed open: %v", err)
			}
			await(t, ended, "the ledger check did not close the daemon peer")
		})
	}
}

func TestExpiryDoesNotWaitForTheLedgerRead(t *testing.T) {
	env := newTokenEnv(t)
	token, err := IssueToken([]byte(testSecret), env.tokens, "ci", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := verify([]byte(testSecret), token.Token)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := newLedger(env.tokens)
	if err != nil {
		t.Fatal(err)
	}
	ledger.mu.Lock()
	verified.ExpiresAt = jwt.NewNumericDate(time.Now().Add(3 * time.Second))
	ctx, stop := (&Server{tokens: ledger}).guard(t.Context(), verified)
	defer func() {
		ledger.mu.Unlock()
		stop()
	}()
	select {
	case <-ctx.Done():
		if !strings.Contains(context.Cause(ctx).Error(), "expired") {
			t.Fatalf("the connection ended for %v, want expiry", context.Cause(ctx))
		}
	case <-time.After(4 * time.Second):
		t.Fatal("expiry waited for the ledger read")
	}
}

func TestNormalTeardownStopsTheTokenGuard(t *testing.T) {
	ledger, err := newLedger(filepath.Join(shortRoot(t), "missing-ledger"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := (&Server{tokens: ledger}).guard(t.Context(), claims{})
	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	await(t, stopped, "the guard did not stop with the proxy")
	if !errors.Is(context.Cause(ctx), context.Canceled) {
		t.Fatalf("normal teardown ended for %v", context.Cause(ctx))
	}
}
