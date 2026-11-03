package sandbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/serve"
)

type forkScopesCase struct {
	name    string
	secrets []string
	policy  string
	scopes  []string
	need    string
	forge   bool
}

func TestForkRequiresScopesForCopiedGrants(t *testing.T) {
	cases := []forkScopesCase{
		{name: "bare source", scopes: []string{models.ScopeSandboxWrite}},
		{name: "secret refused", secrets: []string{"TOKEN"}, scopes: []string{models.ScopeSandboxWrite}, need: models.ScopeSecret},
		{name: "policy refused", policy: "locked", scopes: []string{models.ScopeSandboxWrite}, need: models.ScopePolicy},
		{name: "secret still required", secrets: []string{"TOKEN"}, policy: "locked", scopes: []string{models.ScopeSandboxWrite, models.ScopePolicy}, need: models.ScopeSecret},
		{name: "policy still required", secrets: []string{"TOKEN"}, policy: "locked", scopes: []string{models.ScopeSandboxWrite, models.ScopeSecret}, need: models.ScopePolicy},
		{name: "both allowed", secrets: []string{"TOKEN"}, policy: "locked", scopes: []string{models.ScopeSandboxWrite, models.ScopeSecret, models.ScopePolicy}},
		{name: "wildcard allowed", secrets: []string{"TOKEN"}, policy: "locked", scopes: []string{models.ScopeAll}},
		{name: "forged header refused", secrets: []string{"TOKEN"}, scopes: []string{models.ScopeSandboxWrite}, need: models.ScopeSecret, forge: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { forkWithScopes(t, tc) })
	}
}

func TestForkCheckUsesTheLockedSourceBeforeAnyClaim(t *testing.T) {
	r := &recorder{}
	svc, layers := newService(t, r, forkSource())
	layers.repo.onGet = func() { layers.repo.sb.Secrets = []string{"TOKEN"} }
	denied := errors.New("the copied grant is refused")
	ctx := sandbox.WithForkCheck(t.Context(), func(source models.Sandbox) error {
		if !slices.Equal(source.Secrets, []string{"TOKEN"}) {
			t.Fatalf("the check received an earlier source: %+v", source)
		}
		busy, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
		defer cancel()
		if _, err := svc.Stop(busy, "sandbox1"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the source was not locked during the check: %v", err)
		}
		return denied
	})
	if _, err := svc.Fork(ctx, "sandbox1", sandbox.CopyRequest{}); !errors.Is(err, denied) {
		t.Fatalf("fork did not return the check's refusal: %v", err)
	}
	if calls := keep(r.calls, "repo.Create", "net.Allocate", "provider.Fork"); len(calls) != 0 {
		t.Fatalf("a refused fork claimed or captured a copy: %v", calls)
	}
}

func TestForkOnTheLocalSocketKeepsEveryRight(t *testing.T) {
	source := forkSource()
	source.Secrets, source.Policy = []string{"TOKEN"}, "locked"
	svc, layers := newService(t, &recorder{}, source)
	handler := api.NewHandler("test", nil, layers.repo, nil, svc, nil, nil, nil, io.Discard)
	root := forkAPISocket(t, handler)
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(root, "shard.sock"))
	}}}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost/v0/sandboxes/sandbox1/fork", nil)
	if err != nil {
		t.Fatal(err)
	}
	status, body := forkResponse(t, client, req)
	if status != http.StatusCreated {
		t.Fatalf("a local fork answered %d %v", status, body)
	}
	if !slices.Equal(layers.repo.created.Secrets, source.Secrets) || layers.repo.created.Policy != source.Policy {
		t.Fatalf("a local fork dropped a grant: %+v", layers.repo.created)
	}
}

func forkWithScopes(t *testing.T, tc forkScopesCase) {
	t.Helper()
	source := forkSource()
	source.Secrets, source.Policy = tc.secrets, tc.policy
	r := &recorder{}
	svc, layers := newService(t, r, source)
	handler := api.NewHandler("test", nil, layers.repo, nil, svc, nil, nil, nil, io.Discard)
	root := forkAPISocket(t, handler)
	address, token := forkFront(t, root, tc.scopes)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, address+"/v0/sandboxes/sandbox1/fork", strings.NewReader(`{"name":"copy"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if tc.forge {
		req.Header.Set(api.ScopesHeader, models.ScopeAll)
	}
	status, body := forkResponse(t, http.DefaultClient, req)
	if tc.need != "" {
		forkRefused(t, status, body, tc.need, r)
		return
	}
	if status != http.StatusCreated {
		t.Fatalf("fork answered %d %v", status, body)
	}
	if !slices.Equal(layers.repo.created.Secrets, tc.secrets) || layers.repo.created.Policy != tc.policy {
		t.Fatalf("fork did not retain the allowed grants: %+v", layers.repo.created)
	}
}

func forkRefused(t *testing.T, status int, body map[string]any, need string, r *recorder) {
	t.Helper()
	if status != http.StatusForbidden {
		t.Fatalf("fork answered %d %v, want 403", status, body)
	}
	failure, ok := body["error"].(map[string]any)
	if !ok || failure["code"] != "forbidden" {
		t.Fatalf("fork returned no forbidden error: %v", body)
	}
	message, ok := failure["message"].(string)
	if !ok || !strings.Contains(message, need) {
		t.Fatalf("fork did not name the missing scope %q: %v", need, body)
	}
	if calls := keep(r.calls, "repo.Create", "net.Allocate", "provider.Fork"); len(calls) != 0 {
		t.Fatalf("a refused fork claimed or captured a copy: %v", calls)
	}
}

func forkResponse(t *testing.T, client *http.Client, req *http.Request) (int, map[string]any) {
	t.Helper()
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, body
}

func forkAPISocket(t *testing.T, handler http.Handler) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "shard-fork-") //nolint:usetesting // t.TempDir is too long for a socket path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	listener, err := net.Listen("unix", filepath.Join(root, "shard.sock"))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	ended := make(chan error, 1)
	go func() { ended <- server.Serve(listener) }()
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Error(err)
		}
		if err := <-ended; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("the API server ended: %v", err)
		}
	})
	return root
}

func forkFront(t *testing.T, root string, scopes []string) (string, string) {
	t.Helper()
	key, path, err := serve.SigningKey(root, "")
	if err != nil {
		t.Fatal(err)
	}
	token, err := serve.IssueToken(key, serve.TokensPath(path), "test", scopes, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	front, err := serve.New(serve.Config{Root: root, Listen: "127.0.0.1:0", Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := front.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	ended := make(chan error, 1)
	go func() { ended <- front.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		if err := <-ended; err != nil {
			t.Error(err)
		}
	})
	return "http://" + listener.Addr().String(), token.Token
}
