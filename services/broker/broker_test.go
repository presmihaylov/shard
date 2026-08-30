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

type fakeEvents struct{ recorded []models.EgressEvent }

func (f *fakeEvents) Record(ev models.EgressEvent) error {
	f.recorded = append(f.recorded, ev)

	return nil
}

func newBroker(t *testing.T, sb models.Sandbox, eff egress.Effective) *Service {
	t.Helper()

	return newBrokerWithEvents(t, sb, eff, &fakeEvents{})
}

func newBrokerWithEvents(t *testing.T, sb models.Sandbox, eff egress.Effective, events Events) *Service {
	t.Helper()

	return New(
		fakeRecords{sb},
		fakePolicies{effective: map[string]egress.Effective{sb.ID: eff}, addrs: map[string][]netip.Addr{"api.example.com": {api}, "other.example.com": {api}}},
		fakeSecrets{"TOKEN": {Name: "TOKEN", Destinations: []string{"api.example.com"}, MockValue: "mock-TOKEN"}},
		events,
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
	if r.ContentLength != int64(len(body)) {
		t.Errorf("the length is %d, want %d", r.ContentLength, len(body))
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
	denyCIDR := func(cidr string) egress.Effective {
		return egress.Effective{Policy: "locked", Rules: []egress.EffectiveRule{
			{Rule: models.Rule{Action: models.ActionDeny, Destination: models.Destination{Kind: models.DestinationCIDR, Value: cidr}, Protocol: "tcp"}},
			{Rule: models.Rule{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationGroup, Value: "any"}, Protocol: "tcp"}},
		}}
	}

	_, err := newBroker(t, running(), denyCIDR(api.String()+"/32")).Route(t.Context(), source, "api.example.com", 443)
	var denied *proxy.Denied
	if !errors.As(err, &denied) || !strings.Contains(denied.Reason, "policy locked denies") {
		t.Errorf("Route = %v, want a denial by the address the name resolved to", err)
	}

	if _, err := newBroker(t, running(), denyCIDR("198.51.100.0/24")).Route(t.Context(), source, "api.example.com", 443); err != nil {
		t.Errorf("Route = %v, want the resolved address to pass a deny of another one", err)
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

func TestAChunkedBodyIsRewrittenAndGivenALength(t *testing.T) {
	b := newBroker(t, running("TOKEN"), egress.Effective{})

	route, err := b.Route(t.Context(), source, "api.example.com", 443)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}

	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://api.example.com/v1", io.NopCloser(strings.NewReader(`{"key":"mock-TOKEN"}`)))
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	if r.ContentLength != 0 {
		t.Fatalf("the request has a known length %d, want an unknown one", r.ContentLength)
	}
	if err := route.Rewrite(r); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	body, _ := io.ReadAll(r.Body)
	if want := `{"key":"real-TOKEN"}`; string(body) != want {
		t.Errorf("the body reads %q, want %q", body, want)
	}
	if r.ContentLength != int64(len(body)) {
		t.Errorf("ContentLength is %d, want %d", r.ContentLength, len(body))
	}
}

func TestEveryDecisionIsRecordedWithTheRuleThatMadeIt(t *testing.T) {
	events := &fakeEvents{}
	policy := egress.Effective{Policy: "locked", Rules: []egress.EffectiveRule{
		{Rule: models.Rule{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationDomain, Value: "api.example.com"}, Protocol: "tcp", Ports: []int{80, 443}}},
	}}
	b := newBrokerWithEvents(t, running(), policy, events)

	if _, err := b.Route(t.Context(), source, "api.example.com", 443); err != nil {
		t.Fatalf("Route: %v", err)
	}
	if _, err := b.Route(t.Context(), source, "other.example.com", 443); err == nil {
		t.Fatal("Route allowed a host the policy names nowhere")
	}
	if _, err := b.Route(t.Context(), source, "nowhere.example.com", 443); err == nil {
		t.Fatal("Route allowed a host that does not resolve")
	}

	if len(events.recorded) != 3 {
		t.Fatalf("%d events were recorded, want 3", len(events.recorded))
	}

	allowed := events.recorded[0]
	if allowed.Verdict != models.ActionAllow || allowed.Rule != "0" || allowed.RuleText != "allow domain:api.example.com tcp:80,443" || allowed.Destination != "api.example.com:443" || allowed.Address != api.String() {
		t.Errorf("the allow reads %+v", allowed)
	}
	if denied := events.recorded[1]; denied.Verdict != models.ActionDeny || denied.Rule != egress.RuleDefault || denied.Sandbox != "sb" || denied.Source != models.EgressSourceProxy {
		t.Errorf("the deny reads %+v", denied)
	}
	if unresolved := events.recorded[2]; unresolved.Verdict != models.ActionDeny || unresolved.Rule != egress.RuleResolve || unresolved.Address != "" {
		t.Errorf("the failed resolve reads %+v", unresolved)
	}
}

type failingEvents struct{}

func (failingEvents) Record(models.EgressEvent) error { return errors.New("disk full") }

func TestAnEventThatCannotBeWrittenClosesTheDoor(t *testing.T) {
	b := newBrokerWithEvents(t, running(), egress.Effective{}, failingEvents{})

	_, err := b.Route(t.Context(), source, "api.example.com", 443)
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("Route = %v, want the write error", err)
	}
}
