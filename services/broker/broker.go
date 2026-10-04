// Package broker is the proxy's director: it names the sandbox behind a connection, judges the request by
// the same rules the host enforces, and puts a secret value where the guest put a placeholder in a request header.
package broker

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/dns"
	"github.com/presmihaylov/shard/pkg/proxy"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/network"
	"github.com/presmihaylov/shard/services/sandboxstate"
	"github.com/presmihaylov/shard/services/secret"
)

// Records is the sandbox repository the broker reads; generation moves when the set changes, so the broker lists only then and serves every other request from its own map (SHARD-381).
type Records interface {
	List() ([]models.Sandbox, error)
	Generation() uint64
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

	mu        sync.Mutex
	cachedGen uint64
	byAddr    map[netip.Addr]models.Sandbox
}

func New(records Records, egress *egress.Service, secrets Secrets, log Log) *Broker {
	return &Broker{records: records, egress: egress, secrets: secrets, log: log}
}

// Decide names the sandbox by its address, judges the host by name first so a host the policy never allows is
// refused unresolved, then resolves and asks the policy about that address (SHARD-342).
func (b *Broker) Decide(ctx context.Context, req proxy.Request) (proxy.Decision, error) {
	sb, err := b.sandbox(req.Source)
	if err != nil {
		return proxy.Decision{}, err
	}

	// Judge the name before any lookup, so a host no rule allows never reaches the resolver and no resolved
	// address rides back in the reason (SHARD-342).
	decision, final, err := b.egress.Unresolved(sb, req.Host, req.Port)
	if err != nil {
		return proxy.Decision{}, err
	}
	if final {
		return b.denied(sb.ID, req, decision)
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

	decision, err = b.egress.Decide(sb, req.Host, req.Port, addrs[0])
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

	// The floor, the default and a missing policy have no rule text, so the 403 and the proxy log name the id instead (SHARD-229).
	return proxy.Decision{
		Allowed:  decision.Action == models.ActionAllow,
		Upstream: upstream,
		Rule:     cmp.Or(rule, decision.ID),
		Reason:   decision.Reason,
	}, nil
}

// denied records and returns a deny reached before any lookup, so its record carries no resolved address and
// the 403 reason names only the host the guest sent (SHARD-342).
func (b *Broker) denied(id string, req proxy.Request, decision egress.Decision) (proxy.Decision, error) {
	rule := ""
	if decision.Rule.Destination.Kind != "" {
		rule = egress.FormatRule(decision.Rule.Rule)
	}

	record := egress.Record{Rule: decision.ID, RuleText: rule, Reason: decision.Reason}
	if err := b.record(id, req, decision.Action, record); err != nil {
		return proxy.Decision{}, err
	}

	return proxy.Decision{Rule: cmp.Or(rule, decision.ID), Reason: decision.Reason}, nil
}

// Resolve judges one DNS question by its name alone, before any resolver is asked, and logs it under source dns.
func (b *Broker) Resolve(_ context.Context, q dns.Question) (bool, error) {
	sb, err := b.sandbox(q.Source)
	if err != nil {
		return false, err
	}

	decision, err := b.egress.DecideName(sb, q.Name)
	if err != nil {
		return false, err
	}

	rule := ""
	if decision.Rule.Destination.Kind != "" {
		rule = egress.FormatRule(decision.Rule.Rule)
	}

	record := egress.Record{
		Time:     time.Now().UTC(),
		Source:   egress.SourceDNS,
		Verdict:  string(decision.Action),
		Host:     q.Name,
		Rule:     decision.ID,
		RuleText: rule,
		Reason:   decision.Reason,
	}
	if err := b.log.Append(sb.ID, record); err != nil {
		return false, fmt.Errorf("log the egress decision of sandbox %s: %w", sb.ID, err)
	}

	return decision.Action == models.ActionAllow, nil
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

// Rewrite puts the value of every secret granted to the host where the guest wrote its placeholder in a request header, on TLS only.
func (b *Broker) Rewrite(_ context.Context, req proxy.Request, out *http.Request, body []byte, reserve proxy.Reserve) ([]byte, error) {
	sb, err := b.sandbox(req.Source)
	if err != nil {
		return nil, err
	}

	// A placeholder that is not substituted still maps to itself, so a longer one cannot be eaten by a shorter.
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
		// Plain HTTP crosses the network in cleartext, so a value goes in on TLS alone.
		if !req.TLS || !granted(sec, req.Host) {
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
	var changes []swap
	for _, s := range swaps {
		pairs = append(pairs, s.placeholder, s.with)
		if s.with != s.placeholder {
			changes = append(changes, s)
		}
	}

	if err := substitute(out, substitution{replacer: strings.NewReplacer(pairs...), changes: changes, reserve: reserve}); err != nil {
		return nil, err
	}

	return body, nil
}

func (b *Broker) sandbox(source netip.Addr) (models.Sandbox, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// The map is rebuilt only when a record changed, so a flood of questions is one map read each, not one list each (SHARD-381).
	if gen := b.records.Generation(); b.byAddr == nil || gen != b.cachedGen {
		byAddr, err := b.addresses()
		if err != nil {
			return models.Sandbox{}, err
		}
		b.byAddr = byAddr
		b.cachedGen = gen
	}

	sb, ok := b.byAddr[source]
	if !ok {
		return models.Sandbox{}, fmt.Errorf("no sandbox holds the address %s", source)
	}

	return sb, nil
}

// addresses maps each address to the sandbox that holds it now.
func (b *Broker) addresses() (map[netip.Addr]models.Sandbox, error) {
	// nil log: the daemon tasks already name a bad record, so a per-request log would only flood (SHARD-343, rate SHARD-347).
	sandboxes, err := sandboxstate.ListReadable(b.records, nil)
	if err != nil {
		return nil, fmt.Errorf("read the sandbox records: %w", err)
	}

	byAddr := make(map[netip.Addr]models.Sandbox, len(sandboxes))
	for _, sb := range sandboxes {
		// A failed create keeps the address its teardown gave back, and the next create may hold it now (SHARD-545).
		if sb.Address.IsValid() && sb.State != models.StateFailed {
			byAddr[sb.Address.Addr()] = sb
		}
	}

	return byAddr, nil
}

func granted(sec secret.Secret, host string) bool {
	for _, dest := range sec.Destinations {
		if egress.MatchHost(dest, host) {
			return true
		}
	}

	return false
}

// swap is a placeholder and what goes out in its place.
type swap struct{ placeholder, with string }

// substitution reserves what each rewrite will hold before it allocates it, since a value can be 8192 times its placeholder (SHARD-348).
type substitution struct {
	replacer *strings.Replacer
	changes  []swap
	reserve  proxy.Reserve
}

// substitute edits every end-to-end header value alone, since an upstream quotes the path, the query and the body back in an error (SHARD-337).
func substitute(out *http.Request, sub substitution) error {
	hop := hopByHop(out.Header)
	for name, values := range out.Header {
		if hop[name] {
			continue
		}
		for i, v := range values {
			swapped, err := sub.rewrite(v)
			if err != nil {
				return err
			}
			values[i] = swapped
		}
	}

	if hop["Authorization"] {
		return nil
	}

	return rewriteBasic(out, sub)
}

// bound is the most a rewrite of s can come to; false when no placeholder that changes occurs in it.
func (sub substitution) bound(s string) (int, bool) {
	length, found := len(s), false
	for _, c := range sub.changes {
		n := strings.Count(s, c.placeholder)
		if n == 0 {
			continue
		}
		found = true
		length += n * max(0, len(c.with)-len(c.placeholder))
	}

	return length, found
}

func (sub substitution) rewrite(s string) (string, error) {
	size, found := sub.bound(s)
	if !found {
		return s, nil
	}
	if err := sub.reserve(size); err != nil {
		return "", fmt.Errorf("put the secrets in: %w", err)
	}

	var out strings.Builder
	out.Grow(size)
	if _, err := sub.replacer.WriteString(&out, s); err != nil {
		return "", fmt.Errorf("put the secrets in: %w", err)
	}

	return out.String(), nil
}

// hopByHop names the headers of the connection, not the request, which the proxy handles itself and can quote back to the guest (SHARD-299).
func hopByHop(header http.Header) map[string]bool {
	hop := map[string]bool{
		"Connection": true, "Proxy-Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
		"Proxy-Authorization": true, "Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true,
	}
	for _, value := range header.Values("Connection") {
		for name := range strings.SplitSeq(value, ",") {
			hop[http.CanonicalHeaderKey(strings.TrimSpace(name))] = true
		}
	}

	return hop
}

// rewriteBasic substitutes inside HTTP Basic auth, which every client encodes before the placeholder can be seen.
func rewriteBasic(out *http.Request, sub substitution) error {
	const scheme = "Basic "

	value := out.Header.Get("Authorization")
	if len(value) <= len(scheme) || !strings.EqualFold(value[:len(scheme)], scheme) {
		return nil
	}

	// A header that is not base64 is the guest's own, and it goes out as it is.
	decoded, err := base64.StdEncoding.DecodeString(value[len(scheme):])
	if err != nil {
		return nil
	}

	// A strange but valid-for-its-client header is left alone: a 400 here would break the guest, not protect it.
	if !strings.Contains(string(decoded), ":") {
		return nil
	}

	swapped, err := sub.rewrite(string(decoded))
	if err != nil {
		return err
	}
	if swapped == string(decoded) {
		return nil
	}

	// The bytes of the value, then the encoding, its string and the header each hold it once.
	encoded := len(scheme) + base64.StdEncoding.EncodedLen(len(swapped))
	if err := sub.reserve(len(swapped) + 3*encoded); err != nil {
		return fmt.Errorf("put the secrets in: %w", err)
	}
	out.Header.Set("Authorization", value[:len(scheme)]+base64.StdEncoding.EncodeToString([]byte(swapped)))

	return nil
}
