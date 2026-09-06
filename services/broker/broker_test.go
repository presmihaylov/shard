package broker

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/proxy"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/network"
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

var (
	source   = netip.MustParseAddr("10.87.0.2")
	upstream = netip.MustParseAddr("93.184.216.34")
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

	resolver := fakeResolver{"api.example.com": {upstream}, "other.example.com": {upstream}, "evil.example.net": {upstream}}
	svc := egress.New(store, records, []netip.Addr{netip.MustParseAddr("1.1.1.1")}, resolver)

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
	if _, err := b.Rewrite(t.Context(), proxy.Request{Source: source, Host: "other.example.com", Port: 443}, out, nil); err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if out.Header.Get("Authorization") != "Bearer real-TOKEN" {
		t.Errorf("the allowed granted host did not get the value: %v", out.Header)
	}
}

func TestRewriteSubstitutesOnlyOnAGrantedHost(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{{ID: "sb", Secrets: []string{"TOKEN", "GONE"}, Address: netip.MustParsePrefix("10.87.0.2/16")}}}
	secrets := fakeSecrets{"TOKEN": {Name: "TOKEN", Placeholder: "mock-TOKEN", Destinations: []string{"*.example.com"}}}
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

func TestRewriteReplacesTheLongestPlaceholderFirst(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{{ID: "sb", Secrets: []string{"TOKEN", "TOKEN_B"}, Address: netip.MustParsePrefix("10.87.0.2/16")}}}
	secrets := fixedSecrets{
		fakeSecrets: fakeSecrets{"TOKEN": {Name: "TOKEN", Placeholder: "mock-TOKEN", Destinations: []string{"api.example.com"}}, "TOKEN_B": {Name: "TOKEN_B", Placeholder: "mock-TOKEN_B", Destinations: []string{"api.example.com"}}},
		values:      map[string]string{"TOKEN": "aaaa", "TOKEN_B": "bbbb"},
	}
	b := newBroker(t, records, secrets)

	out := request(t, http.MethodPost, "https://api.example.com/mock-TOKEN_B/mock-TOKEN?b=mock-TOKEN_B")
	out.Header.Set("Authorization", "Bearer mock-TOKEN_B")

	body, err := b.Rewrite(t.Context(), proxy.Request{Source: source, Host: "api.example.com", Port: 443}, out, []byte(`{"a":"mock-TOKEN","b":"mock-TOKEN_B"}`))
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if out.URL.Path != "/bbbb/aaaa" || out.URL.RawQuery != "b=bbbb" {
		t.Errorf("the url became %s", out.URL)
	}
	if out.Header.Get("Authorization") != "Bearer bbbb" {
		t.Errorf("the headers became %v", out.Header)
	}
	if string(body) != `{"a":"aaaa","b":"bbbb"}` {
		t.Errorf("the body became %s", body)
	}
}

func TestRewriteLeavesASkippedPlaceholderWholeUnderAGrantedPrefix(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{{ID: "sb", Secrets: []string{"TOKEN", "TOKEN_B"}, Address: netip.MustParsePrefix("10.87.0.2/16")}}}
	secrets := fixedSecrets{
		fakeSecrets: fakeSecrets{"TOKEN": {Name: "TOKEN", Placeholder: "mock-TOKEN", Destinations: []string{"api.example.com"}}, "TOKEN_B": {Name: "TOKEN_B", Placeholder: "mock-TOKEN_B", Destinations: []string{"other.example.com"}}},
		values:      map[string]string{"TOKEN": "aaaa", "TOKEN_B": "bbbb"},
	}
	b := newBroker(t, records, secrets)

	out := request(t, http.MethodPost, "https://api.example.com/mock-TOKEN_B/mock-TOKEN")
	out.Header.Set("Authorization", "Bearer mock-TOKEN_B")

	body, err := b.Rewrite(t.Context(), proxy.Request{Source: source, Host: "api.example.com", Port: 443}, out, []byte(`{"a":"mock-TOKEN","b":"mock-TOKEN_B"}`))
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if out.URL.Path != "/mock-TOKEN_B/aaaa" {
		t.Errorf("the url became %s", out.URL)
	}
	if out.Header.Get("Authorization") != "Bearer mock-TOKEN_B" {
		t.Errorf("the headers became %v", out.Header)
	}
	if string(body) != `{"a":"aaaa","b":"mock-TOKEN_B"}` {
		t.Errorf("the body became %s", body)
	}
}

func TestRewriteSwapsACustomPlaceholder(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{{ID: "sb", Secrets: []string{"SHAPED"}, Address: netip.MustParsePrefix("10.87.0.2/16")}}}
	secrets := fakeSecrets{"SHAPED": {Name: "SHAPED", Placeholder: "sk_test_shaped01", Destinations: []string{"api.example.com"}}}
	b := newBroker(t, records, secrets)

	out := request(t, http.MethodPost, "https://api.example.com/v1/chat")
	out.Header.Set("Authorization", "Bearer sk_test_shaped01")
	if _, err := b.Rewrite(t.Context(), proxy.Request{Source: source, Host: "api.example.com", Port: 443}, out, nil); err != nil {
		t.Fatal(err)
	}
	if out.Header.Get("Authorization") != "Bearer real-SHAPED" {
		t.Errorf("the granted host got %v", out.Header)
	}

	out = request(t, http.MethodPost, "https://other.example.com/v1/chat")
	out.Header.Set("Authorization", "Bearer sk_test_shaped01")
	if _, err := b.Rewrite(t.Context(), proxy.Request{Source: source, Host: "other.example.com", Port: 443}, out, nil); err != nil {
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

	out := request(t, http.MethodPost, "https://api.example.com/sk_mock-TOKEN_live01/mock-TOKEN")
	if _, err := b.Rewrite(t.Context(), proxy.Request{Source: source, Host: "api.example.com", Port: 443}, out, nil); err != nil {
		t.Fatal(err)
	}
	if out.URL.Path != "/bbbb/aaaa" {
		t.Errorf("the url became %s", out.URL)
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
			if _, err := b.Rewrite(t.Context(), proxy.Request{Source: source, Host: "api.example.com", Port: 443}, out, nil); err != nil {
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
	if _, err := b.Rewrite(t.Context(), proxy.Request{Source: source, Host: "api.example.com", Port: 443}, out, nil); err != nil {
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
			if _, err := b.Rewrite(t.Context(), proxy.Request{Source: source, Host: tc.host, Port: 443}, out, nil); err != nil {
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
	if _, err := b.Rewrite(t.Context(), proxy.Request{Source: source, Host: "api.example.com", Port: 443}, out, nil); err != nil {
		t.Fatal(err)
	}
	if out.Header.Get("Proxy-Authorization") != sent {
		t.Errorf("the header became %q", out.Header.Get("Proxy-Authorization"))
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

// A name that does not resolve stops the request, and the log says so rather than staying silent.
func TestDecideLogsAHostItCannotResolve(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{{ID: "locked", Policy: "web", Address: netip.MustParsePrefix("10.87.0.2/16")}}}
	b, log := newBrokerLog(t, records, fakeSecrets{}, models.Policy{Name: "web"})

	if _, err := b.Decide(t.Context(), proxy.Request{Source: source, Host: "nowhere.example.com", Port: 443}); err == nil {
		t.Fatal("Decide took a host that does not resolve")
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
}
