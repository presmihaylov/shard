// Package broker is the proxy's director: it names the sandbox behind a connection, judges the request by
// the same rules the host enforces, and puts a secret value where the guest put a placeholder.
package broker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/proxy"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/secret"
)

// Records is the part of the sandbox repository the broker reads, on every request, so a change lands at once.
type Records interface {
	List() ([]models.Sandbox, error)
}

// Secrets is the part of the secret store the broker reads. Value is read per request and held for that request only.
type Secrets interface {
	Get(name string) (secret.Secret, error)
	Value(name string) (string, error)
}

// Broker implements proxy.Director over the stores.
type Broker struct {
	records Records
	egress  *egress.Service
	secrets Secrets
}

func New(records Records, egress *egress.Service, secrets Secrets) *Broker {
	return &Broker{records: records, egress: egress, secrets: secrets}
}

// Decide names the sandbox by its address, resolves the host once, and asks the policy about that address.
func (b *Broker) Decide(ctx context.Context, req proxy.Request) (proxy.Decision, error) {
	sb, err := b.sandbox(req.Source)
	if err != nil {
		return proxy.Decision{}, err
	}

	addrs, err := b.egress.Lookup(ctx, req.Host)
	if err != nil {
		return proxy.Decision{}, err
	}
	upstream := netip.AddrPortFrom(addrs[0], uint16(req.Port)) //nolint:gosec // the port is 80 or 443

	decision, err := b.egress.Decide(sb, req.Host, req.Port, addrs[0])
	if err != nil {
		return proxy.Decision{}, err
	}

	rule := ""
	if decision.Rule.Destination.Kind != "" {
		rule = egress.FormatRule(decision.Rule.Rule)
	}

	return proxy.Decision{
		Allowed:  decision.Action == models.ActionAllow,
		Upstream: upstream,
		Rule:     rule,
		Reason:   decision.Reason,
	}, nil
}

// Rewrite puts the value of every secret granted to the host where the guest wrote its placeholder.
func (b *Broker) Rewrite(_ context.Context, req proxy.Request, out *http.Request, body []byte) ([]byte, error) {
	sb, err := b.sandbox(req.Source)
	if err != nil {
		return nil, err
	}

	// A placeholder that is not substituted still maps to itself, so a longer one cannot be eaten by a shorter.
	type swap struct{ placeholder, with string }
	swaps := make([]swap, 0, len(sb.Secrets))

	for _, name := range sb.Secrets {
		sec, err := b.secrets.Get(name)
		if errors.Is(err, secret.ErrNotFound) {
			// A secret removed with --force leaves a placeholder no request can redeem, and it goes out as it is.
			placeholder := secret.DefaultPlaceholder(name)
			swaps = append(swaps, swap{placeholder, placeholder})

			continue
		}
		if err != nil {
			return nil, err
		}
		if !granted(sec, req.Host) {
			swaps = append(swaps, swap{sec.Placeholder, sec.Placeholder})

			continue
		}

		value, err := b.secrets.Value(name)
		if err != nil {
			return nil, err
		}

		swaps = append(swaps, swap{sec.Placeholder, value})
	}

	// mock-TOKEN sits inside mock-TOKEN_B, so the longest placeholder goes first or a shorter one eats it.
	slices.SortStableFunc(swaps, func(a, b swap) int { return len(b.placeholder) - len(a.placeholder) })

	pairs := make([]string, 0, len(swaps)*2)
	for _, s := range swaps {
		pairs = append(pairs, s.placeholder, s.with)
	}

	return substitute(out, body, strings.NewReplacer(pairs...)), nil
}

func (b *Broker) sandbox(source netip.Addr) (models.Sandbox, error) {
	sandboxes, err := b.records.List()
	if err != nil {
		return models.Sandbox{}, fmt.Errorf("read the sandbox records: %w", err)
	}

	for _, sb := range sandboxes {
		if sb.Address.IsValid() && sb.Address.Addr() == source {
			return sb, nil
		}
	}

	return models.Sandbox{}, fmt.Errorf("no sandbox holds the address %s", source)
}

func granted(sec secret.Secret, host string) bool {
	for _, dest := range sec.Destinations {
		if egress.MatchHost(dest, host) {
			return true
		}
	}

	return false
}

// substitute edits the URL, every header value and the held body; a body that was too long to hold is nil and passes as it is.
func substitute(out *http.Request, body []byte, replacer *strings.Replacer) []byte {
	out.URL.Path = replacer.Replace(out.URL.Path)
	out.URL.RawPath = replacer.Replace(out.URL.RawPath)
	out.URL.RawQuery = replacer.Replace(out.URL.RawQuery)

	for name, values := range out.Header {
		for i, v := range values {
			values[i] = replacer.Replace(v)
		}
		out.Header[name] = values
	}

	if body == nil {
		return nil
	}

	return []byte(replacer.Replace(string(body)))
}
