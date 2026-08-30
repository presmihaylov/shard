package broker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/proxy"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/secret"
)

type fakeRecords []models.Sandbox

func (f fakeRecords) List() ([]models.Sandbox, error) { return f, nil }

type fakePolicies struct {
	effective map[string]egress.Effective
	addrs     map[string][]netip.Addr
}

func (f fakePolicies) Effective(sb models.Sandbox) (egress.Effective, error) {
	return f.effective[sb.ID], nil
}

func (f fakePolicies) Resolve(_ context.Context, host string) ([]netip.Addr, error) {
	addrs, ok := f.addrs[host]
	if !ok {
		return nil, errors.New("resolve " + host + ": no such host")
	}

	return addrs, nil
}

type fakeSecrets map[string]secret.Secret

func (f fakeSecrets) Get(name string) (secret.Secret, error) {
	sec, ok := f[name]
	if !ok {
		return secret.Secret{}, secret.ErrNotFound
	}

	return sec, nil
}

func (f fakeSecrets) Value(name string) (string, error) { return "real-" + name, nil }

var (
	source = netip.MustParseAddr("10.87.0.2")
	api    = netip.MustParseAddr("93.184.216.34")
)

func newBroker(t *testing.T, sb models.Sandbox, eff egress.Effective) *Service {
	t.Helper()

	return New(
		fakeRecords{sb},
		fakePolicies{effective: map[string]egress.Effective{sb.ID: eff}, addrs: map[string][]netip.Addr{"api.example.com": {api}, "other.example.com": {api}}},
		fakeSecrets{"TOKEN": {Name: "TOKEN", Destinations: []string{"api.example.com"}, MockValue: "mock-TOKEN"}},
	)
}

func running(secrets ...string) models.Sandbox {
	return models.Sandbox{ID: "sb", State: models.StateRunning, Address: netip.PrefixFrom(source, 16), Secrets: secrets}
}

func request(t *testing.T, route proxy.Route, body string) *http.Request {
	t.Helper()

	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://api.example.com/v1/mock-TOKEN?key=mock-TOKEN", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	r.Header.Set("Authorization", "Bearer mock-TOKEN")
	r.Header.Set("Content-Length", "0")

	if route.Rewrite != nil {
		if err := route.Rewrite(r); err != nil {
			t.Fatalf("rewrite: %v", err)
		}
	}

	return r
}

func TestAGrantedHostGetsTheValueEverywhereThePlaceholderWas(t *testing.T) {
	b := newBroker(t, running("TOKEN"), egress.Effective{})

	route, err := b.Route(t.Context(), source, "api.example.com", 443)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if want := netip.AddrPortFrom(api, 443); route.Target != want {
		t.Errorf("the target is %s, want %s", route.Target, want)
	}

	r := request(t, route, `{"token":"mock-TOKEN"}`)
	body, _ := io.ReadAll(r.Body)

	for what, got := range map[string]string{
		"path":   r.URL.Path,
		"query":  r.URL.RawQuery,
		"header": r.Header.Get("Authorization"),
		"body":   string(body),
	} {
		if strings.Contains(got, "mock-TOKEN") || !strings.Contains(got, "real-TOKEN") {
			t.Errorf("the %s still reads %q", what, got)
		}
	}
	if r.ContentLength != int64(len(body)) || r.Header.Get("Content-Length") != "22" {
		t.Errorf("the length is %d and the header %q, want %d", r.ContentLength, r.Header.Get("Content-Length"), len(body))
	}
}

func TestAHostWithoutAGrantKeepsThePlaceholder(t *testing.T) {
	b := newBroker(t, running("TOKEN"), egress.Effective{})

	route, err := b.Route(t.Context(), source, "other.example.com", 443)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if route.Rewrite != nil {
		t.Error("a host the grant does not name got a rewrite")
	}
}

func TestABodyOverTheBoundGoesOutUnchanged(t *testing.T) {
	b := newBroker(t, running("TOKEN"), egress.Effective{})

	route, err := b.Route(t.Context(), source, "api.example.com", 443)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}

	big := strings.Repeat("x", maxBody) + "mock-TOKEN"
	r := request(t, route, big)
	body, _ := io.ReadAll(r.Body)
	if string(body) != big {
		t.Error("a body over the bound was changed or cut")
	}
	if r.Header.Get("Authorization") != "Bearer real-TOKEN" {
		t.Error("the header was not rewritten alongside a big body")
	}
}

func TestThePolicyIsAskedWithTheResolvedAddress(t *testing.T) {
	deny := egress.Effective{Policy: "locked", Rules: []egress.EffectiveRule{{Rule: models.Rule{
		Action: models.ActionDeny, Destination: models.Destination{Kind: models.DestinationGroup, Value: "any"}, Protocol: "tcp",
	}}}}
	b := newBroker(t, running(), deny)

	_, err := b.Route(t.Context(), source, "api.example.com", 443)
	var denied *proxy.Denied
	if !errors.As(err, &denied) || !strings.Contains(denied.Reason, "policy locked denies") {
		t.Errorf("Route = %v, want a denial by the policy", err)
	}
}

func TestAnUnknownSourceIsDeniedAndAnUnknownHostIsAnError(t *testing.T) {
	b := newBroker(t, running(), egress.Effective{})

	_, err := b.Route(t.Context(), netip.MustParseAddr("10.87.0.9"), "api.example.com", 443)
	var denied *proxy.Denied
	if !errors.As(err, &denied) || !strings.Contains(denied.Reason, "no sandbox holds") {
		t.Errorf("an unknown source got %v", err)
	}

	_, err = b.Route(t.Context(), source, "nowhere.example.com", 443)
	if err == nil || errors.As(err, &denied) {
		t.Errorf("a name that does not resolve got %v, want a plain error", err)
	}
}
