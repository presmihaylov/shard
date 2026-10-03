package broker

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/netip"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/dns"
	"github.com/presmihaylov/shard/pkg/proxy"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/network"
	"github.com/presmihaylov/shard/services/secret"
)

type fakeRecords struct {
	sandboxes []models.Sandbox
	err       error
	gen       uint64
}

func (f fakeRecords) List() ([]models.Sandbox, error) { return f.sandboxes, f.err }

func (f fakeRecords) Generation() uint64 { return f.gen }

// countingRecords counts List calls and lets a test move the generation, so a test proves the broker reads once per generation, not once per request (SHARD-381).
type countingRecords struct {
	mu        sync.Mutex
	sandboxes []models.Sandbox
	lists     int
	gen       uint64
}

func (c *countingRecords) List() ([]models.Sandbox, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.lists++

	return c.sandboxes, nil
}

func (c *countingRecords) Generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.gen
}

func (c *countingRecords) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.lists
}

// bump replaces the set and moves the generation, the way a durable write does.
func (c *countingRecords) bump(sandboxes []models.Sandbox) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.sandboxes = sandboxes
	c.gen++
}

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

// fixedSecrets answers Value from its own map, for a case where "real-" + name would hide a wrong substitution.
type fixedSecrets struct {
	fakeSecrets
	values map[string]string
}

func (f fixedSecrets) Value(name string) (string, error) {
	value, ok := f.values[name]
	if !ok {
		return "", secret.ErrNotFound
	}

	return value, nil
}

type fakeResolver map[string][]netip.Addr

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	addrs, ok := f[host]
	if !ok {
		return nil, errors.New("no such host")
	}

	return addrs, nil
}

// tripResolver counts and flags every lookup: the name-first path must never resolve a host no rule allows,
// or the attacker's label leaves the box and the resolved address rides back in the reason (SHARD-342).
type tripResolver struct {
	t     *testing.T
	calls int
}

func (r *tripResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.calls++
	r.t.Errorf("resolved %q, but a host the policy does not allow must be refused unresolved", host)

	return []netip.Addr{netip.MustParseAddr("10.10.20.2")}, nil
}

var (
	source   = netip.MustParseAddr("10.87.0.2")
	upstream = netip.MustParseAddr("93.184.216.34")
	gateway  = netip.MustParseAddr("10.87.0.1")
)

// fakeLog keeps what the broker decided, so a test can read the line instead of a file.
type fakeLog struct {
	ids     []string
	records []egress.Record
	err     error
}

func (f *fakeLog) Append(id string, record egress.Record) error {
	if f.err != nil {
		return f.err
	}

	f.ids = append(f.ids, id)
	f.records = append(f.records, record)

	return nil
}

func newBroker(t *testing.T, records Records, secrets Secrets, policies ...models.Policy) *Broker {
	t.Helper()

	b, _ := newBrokerLog(t, records, secrets, policies...)

	return b
}

func newBrokerLog(t *testing.T, records Records, secrets Secrets, policies ...models.Policy) (*Broker, *fakeLog) {
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

	resolver := fakeResolver{"api.example.com": {upstream}, "other.example.com": {upstream}, "evil.example.net": {upstream}, "private.example.net": {netip.MustParseAddr("10.0.0.5")}, "0.0.0.0": {netip.IPv4Unspecified()}}
	svc := egress.New(store, records, gateway, []netip.Addr{netip.MustParseAddr("1.1.1.1")}, resolver)

	log := &fakeLog{}

	return New(records, svc, secrets, log), log
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
	secrets := fakeSecrets{"TOKEN": {Name: "TOKEN", Placeholder: "mock-TOKEN", Destinations: []string{"api.example.com"}}}
	web := models.Policy{Name: "web", Rules: []models.Rule{
		{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationDomain, Value: "api.example.com"}, Protocol: "tcp", Ports: []int{80, 443}},
		{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationDomain, Value: "unresolved.example.org"}, Protocol: "tcp", Ports: []int{80, 443}},
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
	if got.Allowed || got.Rule != network.RuleDefault || !strings.Contains(got.Reason, "no rule") {
		t.Errorf("a host the policy does not name got %+v", got)
	}

	got, err = b.Decide(t.Context(), proxy.Request{Source: netip.MustParseAddr("10.87.0.3"), Host: "evil.example.net", Port: 80})
	if err != nil || !got.Allowed {
		t.Errorf("a sandbox fronted by a secret alone got %+v, %v, want the internet", got, err)
	}

	if _, err := b.Decide(t.Context(), proxy.Request{Source: netip.MustParseAddr("10.87.0.9"), Host: "api.example.com", Port: 80}); err == nil {
		t.Error("an address no sandbox holds was judged")
	}
	if _, err := b.Decide(t.Context(), proxy.Request{Source: source, Host: "unresolved.example.org", Port: 80}); err == nil {
		t.Error("an allowed host that does not resolve was judged")
	}
	if _, err := newBroker(t, fakeRecords{err: errors.New("disk")}, secrets).Decide(t.Context(), proxy.Request{Source: source, Host: "api.example.com", Port: 80}); err == nil {
		t.Error("unreadable records still judged")
	}
}

// A dial to 0.0.0.0 from the host reaches the host's own listeners, so the request is refused and logged first (SHARD-291).
func TestDecideRefusesTheUnspecifiedAddress(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{{ID: "free", Address: netip.MustParsePrefix("10.87.0.2/16")}}}
	b, log := newBrokerLog(t, records, fakeSecrets{})

	got, err := b.Decide(t.Context(), proxy.Request{Source: source, Host: "0.0.0.0", Port: 80})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.Allowed || got.Rule != network.RuleLocal {
		t.Errorf("Decide = %+v, want a local deny", got)
	}
	if len(log.records) != 1 || log.records[0].Rule != network.RuleLocal || log.records[0].Verdict != string(models.ActionDeny) {
		t.Errorf("the log holds %+v, want one local deny", log.records)
	}
}

// A deny-all policy still fronts the sandbox, so a guest can put any name in the Host header. The proxy must
// refuse it by name, before any lookup: the label never leaves the box and no resolved address returns (SHARD-342).
func TestDecideRefusesADenyAllHostWithoutResolving(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{{ID: "fronted", Policy: "lockdown", Address: netip.MustParsePrefix("10.87.0.2/16")}}}
	lockdown := models.Policy{Name: "lockdown", Rules: []models.Rule{{Action: models.ActionDeny, Destination: models.Destination{Kind: models.DestinationGroup, Value: "any"}}}}

	store, err := egress.NewStore(filepath.Join(t.TempDir(), "policies"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(lockdown); err != nil {
		t.Fatal(err)
	}

	resolver := &tripResolver{t: t}
	svc := egress.New(store, records, gateway, []netip.Addr{netip.MustParseAddr("1.1.1.1")}, resolver)
	log := &fakeLog{}
	b := New(records, svc, fakeSecrets{}, log)

	got, err := b.Decide(t.Context(), proxy.Request{Source: source, Host: "0a1402.t.attacker.test", Port: 80})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.Allowed {
		t.Fatal("a deny-all policy allowed a fronted host")
	}
	if resolver.calls != 0 {
		t.Errorf("the host was resolved %d time(s); a deny-all policy must refuse it unresolved", resolver.calls)
	}
	if strings.Contains(got.Reason, "10.10.20.2") || strings.Contains(got.Reason, "resolves to") {
		t.Errorf("the 403 reason carried a resolved address: %q", got.Reason)
	}
	if len(log.records) != 1 || log.records[0].Address != "" {
		t.Errorf("the log carried a resolved address: %+v", log.records)
	}
}

// A grant opens nothing: the policy decides the host, so a denied one never reaches Rewrite.
func TestDecideDeniesAGrantedHostThePolicyDoesNotAllow(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{{ID: "locked", Policy: "web", Secrets: []string{"TOKEN"}, Address: netip.MustParsePrefix("10.87.0.2/16")}}}
	secrets := fakeSecrets{"TOKEN": {Name: "TOKEN", Placeholder: "mock-TOKEN", Destinations: []string{"api.example.com", "other.example.com"}}}
	web := models.Policy{Name: "web", Rules: []models.Rule{
		{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationDomain, Value: "other.example.com"}, Protocol: "tcp", Ports: []int{80, 443}},
		{Action: models.ActionDeny, Destination: models.Destination{Kind: models.DestinationGroup, Value: "any"}},
	}}
	b := newBroker(t, records, secrets, web)

	got, err := b.Decide(t.Context(), proxy.Request{Source: source, Host: "api.example.com", Port: 443, TLS: true})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.Allowed || got.Rule != "deny any" {
		t.Errorf("the granted host the policy does not allow got %+v, want the catch-all deny", got)
	}

	// The host the policy does allow still substitutes: both halves are needed, and both are there.
	got, err = b.Decide(t.Context(), proxy.Request{Source: source, Host: "other.example.com", Port: 443, TLS: true})
	if err != nil || !got.Allowed {
		t.Fatalf("the allowed granted host got %+v, %v", got, err)
	}

	out := request(t, http.MethodGet, "https://other.example.com/")
	out.Header.Set("Authorization", "Bearer mock-TOKEN")
	if _, err := b.Rewrite(t.Context(), proxy.Request{Source: source, Host: "other.example.com", Port: 443, TLS: true}, out, nil, unlimited); err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if out.Header.Get("Authorization") != "Bearer real-TOKEN" {
		t.Errorf("the allowed granted host did not get the value: %v", out.Header)
	}
}

// The value goes into request headers alone, so no body is held for it, not even to a granted host (SHARD-337).
func TestDecideHoldsNoBody(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{{ID: "sb", Secrets: []string{"TOKEN"}, Address: netip.MustParsePrefix("10.87.0.2/16")}}}
	secrets := fakeSecrets{"TOKEN": {Name: "TOKEN", Placeholder: "mock-TOKEN", Destinations: []string{"api.example.com"}}}

	got, err := newBroker(t, records, secrets).Decide(t.Context(), proxy.Request{Source: source, Host: "api.example.com", Port: 443, TLS: true})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !got.Allowed || got.Hold {
		t.Errorf("Decide = %+v, want an allow that holds no body", got)
	}
}

// The guest picks the path, the query and the body, and an upstream quotes them back in an error, so only a header gets the value (SHARD-337).
func TestRewritePutsAValueInRequestHeadersOnly(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{{ID: "sb", Secrets: []string{"TOKEN"}, Address: netip.MustParsePrefix("10.87.0.2/16")}}}
	secrets := fakeSecrets{"TOKEN": {Name: "TOKEN", Placeholder: "mock-TOKEN", Destinations: []string{"api.example.com"}}}
	b := newBroker(t, records, secrets)

	out := request(t, http.MethodPost, "https://api.example.com/v1/mock-TOKEN%2Fa?model=mock-TOKEN")
	out.Header.Set("Authorization", "Bearer mock-TOKEN")
	out.Header.Set("X-Api-Key", "mock-TOKEN")
	sent := `{"model":"mock-TOKEN"}`

	body, err := b.Rewrite(t.Context(), proxy.Request{Source: source, Host: "api.example.com", Port: 443, TLS: true}, out, []byte(sent), unlimited)
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if out.Header.Get("Authorization") != "Bearer real-TOKEN" || out.Header.Get("X-Api-Key") != "real-TOKEN" {
		t.Errorf("the headers became %v, want the value", out.Header)
	}
	if out.URL.Path != "/v1/mock-TOKEN/a" || out.URL.RawPath != "/v1/mock-TOKEN%2Fa" || out.URL.RawQuery != "model=mock-TOKEN" {
		t.Errorf("the url became %s, want the placeholder", out.URL)
	}
	if string(body) != sent {
		t.Errorf("the body became %s, want the placeholder", body)
	}
}

func TestRewriteSubstitutesOnlyOnAGrantedHost(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{{ID: "sb", Secrets: []string{"TOKEN", "GONE"}, Address: netip.MustParsePrefix("10.87.0.2/16")}}}
	secrets := fakeSecrets{"TOKEN": {Name: "TOKEN", Placeholder: "mock-TOKEN", Destinations: []string{"*.example.com"}}}
	b := newBroker(t, records, secrets)

	out := request(t, http.MethodPost, "https://api.example.com/v1/chat")
	out.Header.Set("Authorization", "Bearer mock-TOKEN")
	out.Header.Set("X-Gone", "mock-GONE")

	if _, err := b.Rewrite(t.Context(), proxy.Request{Source: source, Host: "api.example.com", Port: 443, TLS: true}, out, nil, unlimited); err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if out.Header.Get("Authorization") != "Bearer real-TOKEN" || out.Header.Get("X-Gone") != "mock-GONE" {
		t.Errorf("the headers became %v", out.Header)
	}

	out = request(t, http.MethodGet, "https://evil.example.net/")
	out.Header.Set("Authorization", "Bearer mock-TOKEN")
	if _, err := b.Rewrite(t.Context(), proxy.Request{Source: source, Host: "evil.example.net", Port: 443, TLS: true}, out, nil, unlimited); err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if out.Header.Get("Authorization") != "Bearer mock-TOKEN" {
		t.Errorf("a host the grant does not name got the value: %v", out.Header)
	}

	// The proxy streams a body it did not hold, so it comes in as nil and goes out as nil.
	if body, err := b.Rewrite(t.Context(), proxy.Request{Source: source, Host: "api.example.com", Port: 443, TLS: true}, request(t, http.MethodPut, "https://api.example.com/"), nil, unlimited); err != nil || body != nil {
		t.Errorf("a streamed body got %q, %v", body, err)
	}
}

func TestRewriteSubstitutesOnTLSOnly(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{{ID: "sb", Secrets: []string{"TOKEN", "KEY"}, Address: netip.MustParsePrefix("10.87.0.2/16")}}}
	secrets := fakeSecrets{
		"TOKEN": {Name: "TOKEN", Placeholder: "mock-TOKEN", Destinations: []string{"api.example.com"}},
		"KEY":   {Name: "KEY", Placeholder: "mock-KEY", Destinations: []string{"api.example.com"}},
	}

	// No value is stored, so a plain request that reads one fails here.
	plain := newBroker(t, records, fixedSecrets{fakeSecrets: secrets})
	out := request(t, http.MethodPost, "http://api.example.com/v1/chat")
	out.Header.Set("Authorization", basic("api", "mock-TOKEN"))
	out.Header.Set("X-Key", "mock-KEY")

	if _, err := plain.Rewrite(t.Context(), proxy.Request{Source: source, Host: "api.example.com", Port: 80}, out, nil, unlimited); err != nil {
		t.Fatalf("Rewrite over plain http: %v", err)
	}
	if out.Header.Get("Authorization") != basic("api", "mock-TOKEN") || out.Header.Get("X-Key") != "mock-KEY" {
		t.Errorf("the headers over plain http became %v", out.Header)
	}

	out = request(t, http.MethodPost, "https://api.example.com/v1/chat")
	out.Header.Set("Authorization", basic("api", "mock-TOKEN"))
	out.Header.Set("X-Key", "mock-KEY")

	if _, err := newBroker(t, records, secrets).Rewrite(t.Context(), proxy.Request{Source: source, Host: "api.example.com", Port: 443, TLS: true}, out, nil, unlimited); err != nil {
		t.Fatalf("Rewrite over tls: %v", err)
	}
	if out.Header.Get("Authorization") != basic("api", "real-TOKEN") || out.Header.Get("X-Key") != "real-KEY" {
		t.Errorf("the headers over tls became %v", out.Header)
	}
}

func TestRewriteReplacesTheLongestPlaceholderFirst(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{{ID: "sb", Secrets: []string{"TOKEN", "TOKEN_B"}, Address: netip.MustParsePrefix("10.87.0.2/16")}}}
	secrets := fixedSecrets{
		fakeSecrets: fakeSecrets{"TOKEN": {Name: "TOKEN", Placeholder: "mock-TOKEN", Destinations: []string{"api.example.com"}}, "TOKEN_B": {Name: "TOKEN_B", Placeholder: "mock-TOKEN_B", Destinations: []string{"api.example.com"}}},
		values:      map[string]string{"TOKEN": "aaaa", "TOKEN_B": "bbbb"},
	}
	b := newBroker(t, records, secrets)

	out := request(t, http.MethodPost, "https://api.example.com/v1/chat")
	out.Header.Set("Authorization", "Bearer mock-TOKEN_B")
	out.Header.Set("X-Keys", "mock-TOKEN_B/mock-TOKEN")

	if _, err := b.Rewrite(t.Context(), proxy.Request{Source: source, Host: "api.example.com", Port: 443, TLS: true}, out, nil, unlimited); err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if out.Header.Get("Authorization") != "Bearer bbbb" || out.Header.Get("X-Keys") != "bbbb/aaaa" {
		t.Errorf("the headers became %v", out.Header)
	}
}

func TestRewriteLeavesASkippedPlaceholderWholeUnderAGrantedPrefix(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{{ID: "sb", Secrets: []string{"TOKEN", "TOKEN_B"}, Address: netip.MustParsePrefix("10.87.0.2/16")}}}
	secrets := fixedSecrets{
		fakeSecrets: fakeSecrets{"TOKEN": {Name: "TOKEN", Placeholder: "mock-TOKEN", Destinations: []string{"api.example.com"}}, "TOKEN_B": {Name: "TOKEN_B", Placeholder: "mock-TOKEN_B", Destinations: []string{"other.example.com"}}},
		values:      map[string]string{"TOKEN": "aaaa", "TOKEN_B": "bbbb"},
	}
	b := newBroker(t, records, secrets)

	out := request(t, http.MethodPost, "https://api.example.com/v1/chat")
	out.Header.Set("Authorization", "Bearer mock-TOKEN_B")
	out.Header.Set("X-Keys", "mock-TOKEN_B/mock-TOKEN")

	if _, err := b.Rewrite(t.Context(), proxy.Request{Source: source, Host: "api.example.com", Port: 443, TLS: true}, out, nil, unlimited); err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if out.Header.Get("Authorization") != "Bearer mock-TOKEN_B" || out.Header.Get("X-Keys") != "mock-TOKEN_B/aaaa" {
		t.Errorf("the headers became %v", out.Header)
	}
}

func TestRewriteSwapsACustomPlaceholder(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{{ID: "sb", Secrets: []string{"SHAPED"}, Address: netip.MustParsePrefix("10.87.0.2/16")}}}
	secrets := fakeSecrets{"SHAPED": {Name: "SHAPED", Placeholder: "sk_test_shaped01", Destinations: []string{"api.example.com"}}}
	b := newBroker(t, records, secrets)

	out := request(t, http.MethodPost, "https://api.example.com/v1/chat")
	out.Header.Set("Authorization", "Bearer sk_test_shaped01")
	if _, err := b.Rewrite(t.Context(), proxy.Request{Source: source, Host: "api.example.com", Port: 443, TLS: true}, out, nil, unlimited); err != nil {
		t.Fatal(err)
	}
	if out.Header.Get("Authorization") != "Bearer real-SHAPED" {
		t.Errorf("the granted host got %v", out.Header)
	}

	out = request(t, http.MethodPost, "https://other.example.com/v1/chat")
	out.Header.Set("Authorization", "Bearer sk_test_shaped01")
	if _, err := b.Rewrite(t.Context(), proxy.Request{Source: source, Host: "other.example.com", Port: 443, TLS: true}, out, nil, unlimited); err != nil {
		t.Fatal(err)
	}
	if out.Header.Get("Authorization") != "Bearer sk_test_shaped01" {
		t.Errorf("a host the grant does not name got the value: %v", out.Header)
	}
}

func TestRewritePutsACustomPlaceholderThatHoldsADefaultOneFirst(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{{ID: "sb", Secrets: []string{"TOKEN", "SHAPED"}, Address: netip.MustParsePrefix("10.87.0.2/16")}}}
	secrets := fixedSecrets{
		fakeSecrets: fakeSecrets{
			"TOKEN":  {Name: "TOKEN", Placeholder: "mock-TOKEN", Destinations: []string{"api.example.com"}},
			"SHAPED": {Name: "SHAPED", Placeholder: "sk_mock-TOKEN_live01", Destinations: []string{"api.example.com"}},
		},
		values: map[string]string{"TOKEN": "aaaa", "SHAPED": "bbbb"},
	}
	b := newBroker(t, records, secrets)

	out := request(t, http.MethodPost, "https://api.example.com/v1/chat")
	out.Header.Set("X-Keys", "sk_mock-TOKEN_live01/mock-TOKEN")
	if _, err := b.Rewrite(t.Context(), proxy.Request{Source: source, Host: "api.example.com", Port: 443, TLS: true}, out, nil, unlimited); err != nil {
		t.Fatal(err)
	}
	if out.Header.Get("X-Keys") != "bbbb/aaaa" {
		t.Errorf("the headers became %v", out.Header)
	}
}

func basic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// A client encodes Basic auth before the request leaves the guest, so the placeholder is only there decoded.
func TestRewriteSubstitutesInsideBasicAuth(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{{ID: "sb", Secrets: []string{"TOKEN"}, Address: netip.MustParsePrefix("10.87.0.2/16")}}}
	secrets := fakeSecrets{"TOKEN": {Name: "TOKEN", Placeholder: "mock-TOKEN", Destinations: []string{"api.example.com"}}}
	b := newBroker(t, records, secrets)

	for _, tc := range []struct {
		name, sent, want string
	}{
		{"the password part", basic("api", "mock-TOKEN"), basic("api", "real-TOKEN")},
		{"the user part", basic("mock-TOKEN", ""), basic("real-TOKEN", "")},
		{"a lowercase scheme", strings.Replace(basic("api", "mock-TOKEN"), "Basic ", "basic ", 1), strings.Replace(basic("api", "real-TOKEN"), "Basic ", "basic ", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := request(t, http.MethodGet, "https://api.example.com/")
			out.Header.Set("Authorization", tc.sent)

			if strings.Contains(tc.sent, "real-TOKEN") {
				t.Fatalf("the sent header already holds the value: %s", tc.sent)
			}
			if _, err := b.Rewrite(t.Context(), proxy.Request{Source: source, Host: "api.example.com", Port: 443, TLS: true}, out, nil, unlimited); err != nil {
				t.Fatal(err)
			}

			got := out.Header.Get("Authorization")
			if got != tc.want {
				t.Errorf("the header became %q, want %q", got, tc.want)
			}

			decoded, err := base64.StdEncoding.DecodeString(strings.SplitN(got, " ", 2)[1])
			if err != nil {
				t.Fatalf("the header is no longer base64: %v", err)
			}
			if strings.Contains(string(decoded), "mock-TOKEN") {
				t.Errorf("the placeholder went out inside the header: %s", decoded)
			}
		})
	}
}

// Both parts hold a placeholder, and the longest-first order holds inside the decoded string too.
func TestRewriteSubstitutesBothPartsOfBasicAuth(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{{ID: "sb", Secrets: []string{"TOKEN", "TOKEN_B"}, Address: netip.MustParsePrefix("10.87.0.2/16")}}}
	secrets := fixedSecrets{
		fakeSecrets: fakeSecrets{
			"TOKEN":   {Name: "TOKEN", Placeholder: "mock-TOKEN", Destinations: []string{"api.example.com"}},
			"TOKEN_B": {Name: "TOKEN_B", Placeholder: "mock-TOKEN_B", Destinations: []string{"api.example.com"}},
		},
		values: map[string]string{"TOKEN": "aaaa", "TOKEN_B": "bbbb"},
	}
	b := newBroker(t, records, secrets)

	out := request(t, http.MethodGet, "https://api.example.com/")
	out.Header.Set("Authorization", basic("mock-TOKEN_B", "mock-TOKEN"))
	if _, err := b.Rewrite(t.Context(), proxy.Request{Source: source, Host: "api.example.com", Port: 443, TLS: true}, out, nil, unlimited); err != nil {
		t.Fatal(err)
	}
	if out.Header.Get("Authorization") != basic("bbbb", "aaaa") {
		t.Errorf("the header became %q", out.Header.Get("Authorization"))
	}
}

// A header the proxy cannot read, or has no business in, goes out exactly as the guest sent it.
func TestRewriteLeavesAHeaderItCannotSubstituteAlone(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{{ID: "sb", Secrets: []string{"TOKEN"}, Address: netip.MustParsePrefix("10.87.0.2/16")}}}
	secrets := fakeSecrets{"TOKEN": {Name: "TOKEN", Placeholder: "mock-TOKEN", Destinations: []string{"api.example.com"}}}
	b := newBroker(t, records, secrets)

	for _, tc := range []struct{ name, sent, host string }{
		{"malformed base64", "Basic not!base64", "api.example.com"},
		{"no colon in the decoded string", "Basic " + base64.StdEncoding.EncodeToString([]byte("mock-TOKEN")), "api.example.com"},
		{"an empty token", "Basic ", "api.example.com"},
		{"another scheme", "Digest " + base64.StdEncoding.EncodeToString([]byte("api:mock-TOKEN")), "api.example.com"},
		{"a host the grant does not name", basic("api", "mock-TOKEN"), "evil.example.net"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := request(t, http.MethodGet, "https://"+tc.host+"/")
			out.Header.Set("Authorization", tc.sent)
			if _, err := b.Rewrite(t.Context(), proxy.Request{Source: source, Host: tc.host, Port: 443, TLS: true}, out, nil, unlimited); err != nil {
				t.Fatal(err)
			}
			if out.Header.Get("Authorization") != tc.sent {
				t.Errorf("the header became %q, want it byte for byte as sent", out.Header.Get("Authorization"))
			}
		})
	}
}

// Proxy-Authorization is for the proxy, never the upstream, so no value is ever put in it.
func TestRewriteLeavesProxyAuthorizationAlone(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{{ID: "sb", Secrets: []string{"TOKEN"}, Address: netip.MustParsePrefix("10.87.0.2/16")}}}
	secrets := fakeSecrets{"TOKEN": {Name: "TOKEN", Placeholder: "mock-TOKEN", Destinations: []string{"api.example.com"}}}
	b := newBroker(t, records, secrets)

	sent := basic("api", "mock-TOKEN")
	out := request(t, http.MethodGet, "https://api.example.com/")
	out.Header.Set("Proxy-Authorization", sent)
	if _, err := b.Rewrite(t.Context(), proxy.Request{Source: source, Host: "api.example.com", Port: 443, TLS: true}, out, nil, unlimited); err != nil {
		t.Fatal(err)
	}
	if out.Header.Get("Proxy-Authorization") != sent {
		t.Errorf("the header became %q", out.Header.Get("Proxy-Authorization"))
	}
}

// A hop-by-hop header belongs to the connection, and the proxy can quote Upgrade back to the guest, so none gets a value.
func TestRewriteLeavesHopByHopHeadersAlone(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{{ID: "sb", Secrets: []string{"TOKEN"}, Address: netip.MustParsePrefix("10.87.0.2/16")}}}
	secrets := fakeSecrets{"TOKEN": {Name: "TOKEN", Placeholder: "mock-TOKEN", Destinations: []string{"api.example.com"}}}
	b := newBroker(t, records, secrets)

	out := request(t, http.MethodGet, "https://api.example.com/")
	out.Header.Set("Connection", "Upgrade, X-Named, Authorization")
	out.Header.Set("Authorization", basic("api", "mock-TOKEN"))
	out.Header.Set("Upgrade", "mock-TOKEN")
	out.Header.Set("Keep-Alive", "mock-TOKEN")
	out.Header.Set("X-Named", "mock-TOKEN")
	out.Header.Set("X-Api-Key", "mock-TOKEN")
	if _, err := b.Rewrite(t.Context(), proxy.Request{Source: source, Host: "api.example.com", Port: 443, TLS: true}, out, nil, unlimited); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"Upgrade", "Keep-Alive", "X-Named"} {
		if out.Header.Get(name) != "mock-TOKEN" {
			t.Errorf("the hop-by-hop header %s became %q", name, out.Header.Get(name))
		}
	}
	if out.Header.Get("Authorization") != basic("api", "mock-TOKEN") {
		t.Errorf("the Basic header Connection names became %q", out.Header.Get("Authorization"))
	}
	if out.Header.Get("X-Api-Key") != "real-TOKEN" {
		t.Errorf("the end-to-end header became %q, want the value", out.Header.Get("X-Api-Key"))
	}
}

func TestDecideLogsWhatItDecided(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{{ID: "locked", Policy: "web", Address: netip.MustParsePrefix("10.87.0.2/16")}}}
	web := models.Policy{Name: "web", Rules: []models.Rule{
		{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationDomain, Value: "api.example.com"}, Protocol: "tcp", Ports: []int{80, 443}},
	}}
	b, log := newBrokerLog(t, records, fakeSecrets{}, web)

	if _, err := b.Decide(t.Context(), proxy.Request{Source: source, Host: "api.example.com", Port: 443, TLS: true}); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if _, err := b.Decide(t.Context(), proxy.Request{Source: source, Host: "evil.example.net", Port: 80}); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if len(log.records) != 2 || log.ids[0] != "locked" {
		t.Fatalf("the log holds %+v for %v", log.records, log.ids)
	}

	// The id counts the effective rules, and the dns-implied allows come before the policy's own.
	allow := log.records[0]
	if allow.Source != egress.SourceProxy || allow.Verdict != string(models.ActionAllow) {
		t.Errorf("the allow became %+v", allow)
	}
	if allow.Host != "api.example.com" || allow.Port != 443 || allow.Address != upstream.String() || allow.Rule != "3" {
		t.Errorf("the allow became %+v", allow)
	}
	if allow.RuleText != "allow api.example.com tcp:80,443" || allow.Time.IsZero() {
		t.Errorf("the allow became %+v", allow)
	}

	if deny := log.records[1]; deny.Verdict != string(models.ActionDeny) || deny.Rule != network.RuleDefault {
		t.Errorf("the deny became %+v", deny)
	}
}

// A deny with no rule text named an empty rule in the 403, so the floor, the default and a missing policy name their id (SHARD-229).
func TestDecideNamesTheIdWhenNoRuleTextDecided(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{
		{ID: "locked", Policy: "web", Address: netip.MustParsePrefix("10.87.0.2/16")},
		{ID: "orphan", Policy: "gone", Address: netip.MustParsePrefix("10.87.0.4/16")},
	}}
	web := models.Policy{Name: "web", Rules: []models.Rule{
		{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationDomain, Value: "api.example.com"}, Protocol: "tcp", Ports: []int{80, 443}},
		{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationDomain, Value: "private.example.net"}, Protocol: "tcp", Ports: []int{80, 443}},
	}}
	b, log := newBrokerLog(t, records, fakeSecrets{}, web)

	for i, tc := range []struct {
		name   string
		source netip.Addr
		host   string
		rule   string
	}{
		{"the floor", source, "private.example.net", network.RulePrivate},
		{"the default", source, "evil.example.net", network.RuleDefault},
		{"a missing policy", netip.MustParseAddr("10.87.0.4"), "api.example.com", network.RuleMissing},
	} {
		got, err := b.Decide(t.Context(), proxy.Request{Source: tc.source, Host: tc.host, Port: 80})
		if err != nil {
			t.Fatalf("%s: Decide: %v", tc.name, err)
		}
		if got.Allowed || got.Rule != tc.rule {
			t.Errorf("%s got %+v, want rule %s", tc.name, got, tc.rule)
		}
		if len(log.records) != i+1 {
			t.Fatalf("%s left the log at %+v", tc.name, log.records)
		}
		if rec := log.records[i]; rec.Rule != tc.rule || rec.RuleText != "" {
			t.Errorf("%s was logged as %+v, want rule %s and no text", tc.name, rec, tc.rule)
		}
	}
}

// A name that does not resolve stops the request, and the log says so rather than staying silent.
func TestDecideLogsAHostItCannotResolve(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{{ID: "locked", Policy: "web", Address: netip.MustParsePrefix("10.87.0.2/16")}}}
	web := models.Policy{Name: "web", Rules: []models.Rule{
		{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationDomain, Value: "nowhere.example.com"}, Protocol: "tcp", Ports: []int{80, 443}},
	}}
	b, log := newBrokerLog(t, records, fakeSecrets{}, web)

	if _, err := b.Decide(t.Context(), proxy.Request{Source: source, Host: "nowhere.example.com", Port: 443}); err == nil {
		t.Fatal("Decide took an allowed host that does not resolve")
	}
	if len(log.records) != 1 || log.records[0].Rule != network.RuleResolve || log.records[0].Verdict != string(models.ActionDeny) {
		t.Errorf("the log holds %+v", log.records)
	}
}

// An unlogged decision would make the log a half-truth, so a log that refuses closes the door.
func TestDecideRefusesWhenTheLogRefuses(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{{ID: "locked", Policy: "web", Address: netip.MustParsePrefix("10.87.0.2/16")}}}
	b, log := newBrokerLog(t, records, fakeSecrets{}, models.Policy{Name: "web"})
	log.err = errors.New("the disk is full")

	if _, err := b.Decide(t.Context(), proxy.Request{Source: source, Host: "api.example.com", Port: 443}); err == nil {
		t.Fatal("Decide answered with no log")
	}
	if _, err := b.Resolve(t.Context(), dns.Question{Source: source, Name: "api.example.com"}); err == nil {
		t.Fatal("Resolve answered with no log")
	}
}

func TestResolveJudgesTheNameAloneAndLogsIt(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{{ID: "locked", Policy: "web", Address: netip.MustParsePrefix("10.87.0.2/16")}}}
	web := models.Policy{Name: "web", Rules: []models.Rule{
		{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationDomain, Value: "api.example.com"}, Protocol: "tcp", Ports: []int{80, 443}},
	}}
	b, log := newBrokerLog(t, records, fakeSecrets{}, web)

	allowed, err := b.Resolve(t.Context(), dns.Question{Source: source, Name: "api.example.com"})
	if err != nil || !allowed {
		t.Fatalf("Resolve = %v, %v, want the name the policy allows", allowed, err)
	}
	// The name is refused before any lookup, so the resolver never learns whether it exists.
	allowed, err = b.Resolve(t.Context(), dns.Question{Source: source, Name: "evil.example.net"})
	if err != nil || allowed {
		t.Fatalf("Resolve = %v, %v, want a name the policy never allows refused", allowed, err)
	}

	if len(log.records) != 2 || log.ids[0] != "locked" || log.ids[1] != "locked" {
		t.Fatalf("the log holds %+v for %v", log.records, log.ids)
	}

	allow := log.records[0]
	if allow.Source != egress.SourceDNS || allow.Verdict != string(models.ActionAllow) || allow.Time.IsZero() {
		t.Errorf("the allow became %+v", allow)
	}
	if allow.Host != "api.example.com" || allow.Port != 0 || allow.Address != "" || allow.Rule != "3" || allow.RuleText != "allow api.example.com tcp:80,443" {
		t.Errorf("the allow became %+v", allow)
	}

	deny := log.records[1]
	if deny.Source != egress.SourceDNS || deny.Verdict != string(models.ActionDeny) || deny.Host != "evil.example.net" || deny.Rule != network.RuleDefault {
		t.Errorf("the deny became %+v", deny)
	}
}

// A deny-all guest can flood DNS, so the broker reads the records once per generation, not once per question (SHARD-381).
func TestResolveReadsTheRecordsOncePerGeneration(t *testing.T) {
	records := &countingRecords{sandboxes: []models.Sandbox{{ID: "sb", Address: netip.MustParsePrefix("10.87.0.2/16")}}}
	b := newBroker(t, records, fakeSecrets{})

	for range 100 {
		if _, err := b.Resolve(t.Context(), dns.Question{Source: source, Name: "api.example.com"}); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
	}
	if got := records.calls(); got != 1 {
		t.Fatalf("the broker listed the records %d times for 100 questions, want 1 (SHARD-381)", got)
	}

	// A new record moves the generation, so the next question rebuilds the map once and finds the new address.
	records.bump([]models.Sandbox{
		{ID: "sb", Address: netip.MustParsePrefix("10.87.0.2/16")},
		{ID: "new", Address: netip.MustParsePrefix("10.87.0.3/16")},
	})
	if _, err := b.Resolve(t.Context(), dns.Question{Source: netip.MustParseAddr("10.87.0.3"), Name: "api.example.com"}); err != nil {
		t.Fatalf("Resolve after a new record: %v", err)
	}
	if got := records.calls(); got != 2 {
		t.Fatalf("the broker listed the records %d times, want 2 after the generation moved", got)
	}
}

// An address alone gives a guest nothing to resolve, and a sandbox with no policy is never refused a name.
func TestResolveReadsThePolicyOfTheSandboxThatAsked(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{
		{ID: "locked", Policy: "addresses", Address: netip.MustParsePrefix("10.87.0.2/16")},
		{ID: "open", Address: netip.MustParsePrefix("10.87.0.3/16")},
	}}
	addresses := models.Policy{Name: "addresses", Rules: []models.Rule{
		{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationCIDR, Value: "203.0.113.7/32"}},
	}}
	b, log := newBrokerLog(t, records, fakeSecrets{}, addresses)

	allowed, err := b.Resolve(t.Context(), dns.Question{Source: source, Name: "api.example.com"})
	if err != nil || allowed {
		t.Errorf("Resolve = %v, %v under a policy of addresses only, want refused", allowed, err)
	}
	allowed, err = b.Resolve(t.Context(), dns.Question{Source: netip.MustParseAddr("10.87.0.3"), Name: "api.example.com"})
	if err != nil || !allowed {
		t.Errorf("Resolve = %v, %v with no policy, want allowed", allowed, err)
	}
	if len(log.records) != 2 || log.records[0].Rule != network.RuleDefault || log.records[1].Rule != network.RuleNone {
		t.Errorf("the log holds %+v", log.records)
	}

	if _, err := b.Resolve(t.Context(), dns.Question{Source: netip.MustParseAddr("10.87.0.9"), Name: "api.example.com"}); err == nil {
		t.Error("Resolve answered a question from an address no sandbox holds")
	}
}

func unlimited(int) error { return nil }

var errCapped = errors.New("the test reserve is spent")

// capped is a reserve that refuses past its limit and counts what it granted.
type capped struct{ limit, granted int }

func (c *capped) reserve(n int) error {
	if c.granted+n > c.limit {
		return fmt.Errorf("%w at %d bytes", errCapped, c.limit)
	}
	c.granted += n

	return nil
}

// A value of 64 KiB behind a placeholder of 8 bytes turns 1 KiB of request into 8 MiB, so the reserve must refuse it before it exists (SHARD-348).
func TestRewriteReservesWhatAValueAddsBeforeItAllocates(t *testing.T) {
	const placeholder = "mock-SHO"
	value := strings.Repeat("v", 64<<10)
	many := strings.Repeat(placeholder, 128)
	records := fakeRecords{sandboxes: []models.Sandbox{{ID: "sb", Secrets: []string{"SHO"}, Address: netip.MustParsePrefix("10.87.0.2/16")}}}
	secrets := fixedSecrets{
		fakeSecrets: fakeSecrets{"SHO": {Name: "SHO", Placeholder: placeholder, Destinations: []string{"api.example.com"}}},
		values:      map[string]string{"SHO": value},
	}
	b := newBroker(t, records, secrets)
	req := proxy.Request{Source: source, Host: "api.example.com", Port: 443, TLS: true}

	for _, tc := range []struct {
		name   string
		header http.Header
	}{
		{name: "header", header: http.Header{"X-Key": {many}}},
		{name: "basic", header: http.Header{"Authorization": {"Basic " + base64.StdEncoding.EncodeToString([]byte("u:"+many))}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := request(t, http.MethodPost, "https://api.example.com/v1")
			maps.Copy(out.Header, tc.header)
			limit := &capped{limit: 1 << 20}

			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			body, err := b.Rewrite(t.Context(), req, out, nil, limit.reserve)
			runtime.ReadMemStats(&after)

			if !errors.Is(err, errCapped) || body != nil {
				t.Fatalf("Rewrite = %d bytes, %v; want the reserve's refusal", len(body), err)
			}
			if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
				t.Errorf("the refused rewrite allocated %d bytes", grew)
			}
		})
	}

	out := request(t, http.MethodPost, "https://api.example.com/v1")
	out.Header.Set("X-Key", many)
	limit := &capped{limit: 64 << 20}
	if _, err := b.Rewrite(t.Context(), req, out, nil, limit.reserve); err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if out.Header.Get("X-Key") != strings.Repeat(value, 128) {
		t.Fatalf("the rewrite put in the wrong bytes: a header of %d", len(out.Header.Get("X-Key")))
	}
	if held := len(out.Header.Get("X-Key")); limit.granted < held {
		t.Errorf("the rewrite reserved %d bytes and holds %d", limit.granted, held)
	}
}
