package broker

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/proxy"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/secret"
)

type fakeRecords struct {
	sandboxes []models.Sandbox
	err       error
}

func (f fakeRecords) List() ([]models.Sandbox, error) { return f.sandboxes, f.err }

type fakeSecrets map[string]secret.Secret

func (f fakeSecrets) Get(name string) (secret.Secret, error) {
	sec, ok := f[name]
	if !ok {
		return secret.Secret{}, secret.ErrNotFound
	}

	return sec, nil
}

func (f fakeSecrets) Value(name string) (string, error) {
	if _, ok := f[name]; !ok {
		return "", secret.ErrNotFound
	}

	return "real-" + name, nil
}

type fakeResolver map[string][]netip.Addr

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	addrs, ok := f[host]
	if !ok {
		return nil, errors.New("no such host")
	}

	return addrs, nil
}

var (
	source   = netip.MustParseAddr("10.87.0.2")
	upstream = netip.MustParseAddr("93.184.216.34")
)

func newBroker(t *testing.T, records Records, secrets fakeSecrets, policies ...models.Policy) *Broker {
	t.Helper()

	store, err := egress.NewStore(filepath.Join(t.TempDir(), "policies"))
	if err != nil {
		t.Fatal(err)
	}
	for _, policy := range policies {
		if err := store.Set(policy); err != nil {
			t.Fatal(err)
		}
	}

	resolver := fakeResolver{"api.example.com": {upstream}, "other.example.com": {upstream}, "evil.example.net": {upstream}}
	svc := egress.New(store, records, secrets, []netip.Addr{netip.MustParseAddr("1.1.1.1")}, resolver)

	return New(records, svc, secrets)
}

func request(t *testing.T, method, url string) *http.Request {
	t.Helper()

	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}

	return req
}

func TestDecideNamesTheSandboxByAddressAndPinsTheUpstream(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{
		{ID: "locked", Policy: "web", Address: netip.MustParsePrefix("10.87.0.2/16")},
		{ID: "free", Secrets: []string{"TOKEN"}, Address: netip.MustParsePrefix("10.87.0.3/16")},
	}}
	secrets := fakeSecrets{"TOKEN": {Name: "TOKEN", Destinations: []string{"api.example.com"}}}
	web := models.Policy{Name: "web", Rules: []models.Rule{
		{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationDomain, Value: "api.example.com"}, Protocol: "tcp", Ports: []int{80, 443}},
	}}
	b := newBroker(t, records, secrets, web)

	got, err := b.Decide(t.Context(), proxy.Request{Source: source, Host: "api.example.com", Port: 443, TLS: true})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !got.Allowed || got.Upstream != netip.AddrPortFrom(upstream, 443) || got.Rule != "allow api.example.com tcp:80,443" {
		t.Errorf("Decide = %+v", got)
	}

	got, err = b.Decide(t.Context(), proxy.Request{Source: source, Host: "evil.example.net", Port: 80})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.Allowed || got.Rule != "" || !strings.Contains(got.Reason, "no rule") {
		t.Errorf("a host the policy does not name got %+v", got)
	}

	got, err = b.Decide(t.Context(), proxy.Request{Source: netip.MustParseAddr("10.87.0.3"), Host: "evil.example.net", Port: 80})
	if err != nil || !got.Allowed {
		t.Errorf("a sandbox fronted by a secret alone got %+v, %v, want the internet", got, err)
	}

	if _, err := b.Decide(t.Context(), proxy.Request{Source: netip.MustParseAddr("10.87.0.9"), Host: "api.example.com", Port: 80}); err == nil {
		t.Error("an address no sandbox holds was judged")
	}
	if _, err := b.Decide(t.Context(), proxy.Request{Source: source, Host: "nowhere.example.com", Port: 80}); err == nil {
		t.Error("a host that does not resolve was judged")
	}
	if _, err := newBroker(t, fakeRecords{err: errors.New("disk")}, secrets).Decide(t.Context(), proxy.Request{Source: source, Host: "api.example.com", Port: 80}); err == nil {
		t.Error("unreadable records still judged")
	}
}

func TestRewriteSubstitutesOnlyOnAGrantedHost(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{{ID: "sb", Secrets: []string{"TOKEN", "GONE"}, Address: netip.MustParsePrefix("10.87.0.2/16")}}}
	secrets := fakeSecrets{"TOKEN": {Name: "TOKEN", Destinations: []string{"*.example.com"}}}
	b := newBroker(t, records, secrets)

	out := request(t, http.MethodPost, "https://api.example.com/v1/mock-TOKEN?key=mock-TOKEN&gone=mock-GONE")
	out.Header.Set("Authorization", "Bearer mock-TOKEN")
	out.Header.Set("X-Gone", "mock-GONE")

	body, err := b.Rewrite(t.Context(), proxy.Request{Source: source, Host: "api.example.com", Port: 443}, out, []byte(`{"token":"mock-TOKEN","gone":"mock-GONE"}`))
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if out.URL.Path != "/v1/real-TOKEN" || out.URL.RawQuery != "key=real-TOKEN&gone=mock-GONE" {
		t.Errorf("the url became %s", out.URL)
	}
	if out.Header.Get("Authorization") != "Bearer real-TOKEN" || out.Header.Get("X-Gone") != "mock-GONE" {
		t.Errorf("the headers became %v", out.Header)
	}
	if string(body) != `{"token":"real-TOKEN","gone":"mock-GONE"}` {
		t.Errorf("the body became %s", body)
	}

	out = request(t, http.MethodGet, "https://evil.example.net/mock-TOKEN")
	out.Header.Set("Authorization", "Bearer mock-TOKEN")
	body, err = b.Rewrite(t.Context(), proxy.Request{Source: source, Host: "evil.example.net", Port: 443}, out, []byte("mock-TOKEN"))
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if out.URL.Path != "/mock-TOKEN" || out.Header.Get("Authorization") != "Bearer mock-TOKEN" || string(body) != "mock-TOKEN" {
		t.Errorf("a host the grant does not name got the value: %s %v %s", out.URL, out.Header, body)
	}

	// A body too long to hold comes in as nil and goes out as nil: the proxy streams it unchanged.
	if body, err := b.Rewrite(t.Context(), proxy.Request{Source: source, Host: "api.example.com", Port: 443}, request(t, http.MethodPut, "https://api.example.com/"), nil); err != nil || body != nil {
		t.Errorf("a streamed body got %q, %v", body, err)
	}
}

func TestRewriteSetsTheGrantHeadersWhenTheMatchHolds(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{{ID: "sb", Secrets: []string{"TOKEN"}, Address: netip.MustParsePrefix("10.87.0.2/16")}}}
	secrets := fakeSecrets{"TOKEN": {
		Name:         "TOKEN",
		Destinations: []string{"api.example.com"},
		Headers:      []secret.Header{{Name: "Authorization", Value: "Bearer {value}"}, {Name: "X-Static", Value: "fixed"}},
		Match:        secret.Match{Path: "/v1/", Method: "POST", Query: []string{"team=blue"}, Headers: []string{"X-Env=prod"}},
	}}
	b := newBroker(t, records, secrets)
	req := proxy.Request{Source: source, Host: "api.example.com", Port: 443}

	out := request(t, http.MethodPost, "https://api.example.com/v1/chat?team=blue")
	out.Header.Set("X-Env", "prod")
	out.Header.Set("Authorization", "Bearer guest-said-so")
	if _, err := b.Rewrite(t.Context(), req, out, nil); err != nil {
		t.Fatal(err)
	}
	if out.Header.Get("Authorization") != "Bearer real-TOKEN" || out.Header.Get("X-Static") != "fixed" {
		t.Errorf("a matching request got %v", out.Header)
	}

	for _, miss := range []*http.Request{
		request(t, http.MethodGet, "https://api.example.com/v1/chat?team=blue"),
		request(t, http.MethodPost, "https://api.example.com/v2/chat?team=blue"),
		request(t, http.MethodPost, "https://api.example.com/v1/chat?team=red"),
		request(t, http.MethodPost, "https://api.example.com/v1/chat?team=blue"),
	} {
		if miss.URL.Path == "/v1/chat" && miss.Method == http.MethodPost && miss.URL.RawQuery == "team=blue" {
			miss.Header.Set("X-Env", "staging")
		}
		if miss.Header.Get("X-Env") == "" && miss.Method != http.MethodPost {
			miss.Header.Set("X-Env", "prod")
		}
		miss.Header.Set("Authorization", "Bearer mock-TOKEN")
		if _, err := b.Rewrite(t.Context(), req, miss, nil); err != nil {
			t.Fatal(err)
		}
		// The match gates the headers only: the placeholder is still replaced.
		if out := miss.Header.Get("Authorization"); out != "Bearer real-TOKEN" || miss.Header.Get("X-Static") != "" {
			t.Errorf("%s %s got %v", miss.Method, miss.URL, miss.Header)
		}
	}
}
