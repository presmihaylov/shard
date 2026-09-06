// Package broker is the proxy's director: it names the sandbox behind a connection, judges the request by
// the same rules the host enforces, and puts a secret value where the guest put a placeholder.
package broker

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/proxy"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/network"
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

// Log is where every decision goes. A decision that cannot be written closes the door.
type Log interface {
	Append(id string, record egress.Record) error
}

// Broker implements proxy.Director over the stores.
type Broker struct {
	records Records
	egress  *egress.Service
	secrets Secrets
	log     Log
}

func New(records Records, egress *egress.Service, secrets Secrets, log Log) *Broker {
	return &Broker{records: records, egress: egress, secrets: secrets, log: log}
}

// Decide names the sandbox by its address, resolves the host once, and asks the policy about that address.
func (b *Broker) Decide(ctx context.Context, req proxy.Request) (proxy.Decision, error) {
	sb, err := b.sandbox(req.Source)
	if err != nil {
		return proxy.Decision{}, err
	}

	addrs, err := b.egress.Lookup(ctx, req.Host)
	if err != nil {
		// A name that does not resolve is a decision like any other, and the log says why the request stopped.
		record := egress.Record{Rule: network.RuleResolve, Reason: err.Error()}
		if logErr := b.record(sb.ID, req, models.ActionDeny, record); logErr != nil {
			return proxy.Decision{}, logErr
		}

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

	record := egress.Record{Address: addrs[0].String(), Rule: decision.ID, RuleText: rule, Reason: decision.Reason}
	if err := b.record(sb.ID, req, decision.Action, record); err != nil {
		return proxy.Decision{}, err
	}

	return proxy.Decision{
		Allowed:  decision.Action == models.ActionAllow,
		Upstream: upstream,
		Rule:     rule,
		Reason:   decision.Reason,
	}, nil
}

// record fills in what every line shares and appends it. Its error is returned, never swallowed: an
// unlogged decision would make the log a half-truth, and deny-by-default is only tolerable when it is read.
func (b *Broker) record(id string, req proxy.Request, action models.Action, record egress.Record) error {
	record.Time = time.Now().UTC()
	record.Source = egress.SourceProxy
	record.Verdict = string(action)
	record.Host = req.Host
	record.Port = req.Port

	if err := b.log.Append(id, record); err != nil {
		return fmt.Errorf("log the egress decision of sandbox %s: %w", id, err)
	}

	return nil
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

	rewriteBasic(out, replacer)

	if body == nil {
		return nil
	}

	return []byte(replacer.Replace(string(body)))
}

// rewriteBasic substitutes inside HTTP Basic auth, which every client encodes before the placeholder can be seen.
func rewriteBasic(out *http.Request, replacer *strings.Replacer) {
	const scheme = "Basic "

	value := out.Header.Get("Authorization")
	if len(value) <= len(scheme) || !strings.EqualFold(value[:len(scheme)], scheme) {
		return
	}

	decoded, err := base64.StdEncoding.DecodeString(value[len(scheme):])
	if err != nil {
		return
	}

	// A strange but valid-for-its-client header is left alone: a 400 here would break the guest, not protect it.
	if !strings.Contains(string(decoded), ":") {
		return
	}

	swapped := replacer.Replace(string(decoded))
	if swapped == string(decoded) {
		return
	}

	out.Header.Set("Authorization", value[:len(scheme)]+base64.StdEncoding.EncodeToString([]byte(swapped)))
}
