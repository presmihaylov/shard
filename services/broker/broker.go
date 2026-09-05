// Package broker is the proxy's director: it names the sandbox behind a connection, judges the request by
// the same rules the host enforces, and puts a secret value where the guest put a placeholder.
package broker

import (
	"bytes"
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

// Rewrite replaces the placeholder of every secret granted to the host, then sets the headers the grant asks for.
func (b *Broker) Rewrite(_ context.Context, req proxy.Request, out *http.Request, body []byte) ([]byte, error) {
	sb, err := b.sandbox(req.Source)
	if err != nil {
		return nil, err
	}

	// mock-TOKEN sits inside mock-TOKEN_B, so the longest placeholder goes first or a shorter name eats it.
	names := slices.Clone(sb.Secrets)
	slices.SortStableFunc(names, func(a, b string) int { return len(b) - len(a) })

	for _, name := range names {
		sec, err := b.secrets.Get(name)
		if errors.Is(err, secret.ErrNotFound) {
			// A secret removed with --force leaves a placeholder no request can redeem, and it goes out as it is.
			continue
		}
		if err != nil {
			return nil, err
		}
		if !granted(sec, req.Host) {
			continue
		}

		value, err := b.secrets.Value(name)
		if err != nil {
			return nil, err
		}

		body = substitute(out, body, secret.MockValue(name), value)
		if matches(sec.Match, out) {
			for _, header := range sec.Headers {
				out.Header.Set(header.Name, strings.ReplaceAll(header.Value, "{value}", value))
			}
		}
	}

	return body, nil
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
func substitute(out *http.Request, body []byte, placeholder, value string) []byte {
	replace := func(s string) string { return strings.ReplaceAll(s, placeholder, value) }

	out.URL.Path = replace(out.URL.Path)
	out.URL.RawPath = replace(out.URL.RawPath)
	out.URL.RawQuery = replace(out.URL.RawQuery)

	for name, values := range out.Header {
		for i, v := range values {
			values[i] = replace(v)
		}
		out.Header[name] = values
	}

	if body == nil {
		return nil
	}

	return bytes.ReplaceAll(body, []byte(placeholder), []byte(value))
}

// matches is all-of over what the grant asks: an empty match meets every request.
func matches(match secret.Match, out *http.Request) bool {
	if match.Path != "" && !strings.HasPrefix(out.URL.Path, match.Path) {
		return false
	}
	if match.Method != "" && out.Method != match.Method {
		return false
	}

	query := out.URL.Query()
	for _, pair := range match.Query {
		key, want, _ := strings.Cut(pair, "=")
		if query.Get(key) != want {
			return false
		}
	}
	for _, pair := range match.Headers {
		key, want, _ := strings.Cut(pair, "=")
		if out.Header.Get(key) != want {
			return false
		}
	}

	return true
}
