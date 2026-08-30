package egress

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/kmsg"
)

const (
	eventsFile = "egress.jsonl"
	eventsPerm = 0o600
	// maxLine bounds one record; the broker refuses a host longer than a name can be, so a line stays far under it.
	maxLine = 64 << 10
)

// Events keeps what the proxy decided, one JSON line per request, in the sandbox directory.
type Events struct {
	dir func(id string) (string, error)
}

// NewEvents takes the sandbox directory lookup, since the file lives beside the record.
func NewEvents(dir func(id string) (string, error)) *Events {
	return &Events{dir: dir}
}

// Record appends one event. The write is one line, so a reader in the middle sees whole events only.
func (e *Events) Record(ev models.EgressEvent) error {
	dir, err := e.dir(ev.Sandbox)
	if err != nil {
		return err
	}

	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("encode the egress event: %w", err)
	}

	path := filepath.Join(dir, eventsFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, eventsPerm) // #nosec G304
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}

	_, err = f.Write(append(line, '\n'))

	return errors.Join(err, f.Close())
}

// Read is every proxy event of the sandbox, oldest first. A sandbox the proxy never judged has none.
func (e *Events) Read(id string) ([]models.EgressEvent, error) {
	dir, err := e.dir(id)
	if err != nil {
		return nil, err
	}

	path := filepath.Join(dir, eventsFile)
	f, err := os.Open(path) // #nosec G304
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	var events []models.EgressEvent
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, maxLine), maxLine)
	for scanner.Scan() {
		var ev models.EgressEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
		events = append(events, ev)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	return events, nil
}

// HostEvents turns the kernel log lines the sandbox's drops wrote into events; the host logs no accept.
func HostEvents(lines []kmsg.Line, sb models.Sandbox, eff Effective, prefix string) []models.EgressEvent {
	var events []models.EgressEvent
	for _, line := range lines {
		rest, ok := strings.CutPrefix(line.Message, prefix+" rule=")
		if !ok {
			continue
		}

		// The address is reused once the sandbox is gone, so an older line belongs to whoever held it before.
		if line.Time.Before(sb.CreatedAt) {
			continue
		}

		id, fields, _ := strings.Cut(rest, " ")
		packet := packetFields(fields)
		// The forward chain sees the bridge as IN, never the sandbox's veth, so the source address is what names the sandbox.
		if packet["SRC"] != sb.Address.Addr().String() {
			continue
		}

		ev := models.EgressEvent{
			Time:        line.Time,
			Sandbox:     sb.ID,
			Source:      models.EgressSourceHost,
			Verdict:     models.ActionDeny,
			Protocol:    strings.ToLower(packet["PROTO"]),
			Destination: packet["DST"],
			Address:     packet["DST"],
			Rule:        id,
			RuleText:    ruleText(eff, id),
		}
		if port := packet["DPT"]; port != "" {
			ev.Destination += ":" + port
		}
		events = append(events, ev)
	}

	return events
}

// packetFields reads the KEY=VALUE pairs a netfilter log line carries.
func packetFields(fields string) map[string]string {
	packet := map[string]string{}
	for field := range strings.FieldsSeq(fields) {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		packet[key] = value
	}

	return packet
}

// ruleText is the rule an index names, as shard policy show would print it, or empty when no rule did.
func ruleText(eff Effective, id string) string {
	i, err := strconv.Atoi(id)
	if err != nil || i < 0 || i >= len(eff.Rules) {
		return ""
	}

	return eff.Rules[i].Text()
}

// Text is the rule in the spelling policy create takes, with what implied it when the policy did not write it.
func (r EffectiveRule) Text() string {
	text := fmt.Sprintf("%s %s:%s", r.Action, r.Destination.Kind, r.Destination.Value)
	if r.Protocol != "" {
		text += " " + r.Protocol
	}
	if len(r.Ports) != 0 {
		ports := make([]string, 0, len(r.Ports))
		for _, port := range r.Ports {
			ports = append(ports, strconv.Itoa(port))
		}
		text += ":" + strings.Join(ports, ",")
	}
	if r.Implied != "" {
		text += " (" + r.Implied + ")"
	}

	return text
}

// Merge is both sources in one list, oldest first, for one sandbox.
func Merge(proxied, host []models.EgressEvent) []models.EgressEvent {
	all := slices.Concat(proxied, host)
	slices.SortStableFunc(all, func(a, b models.EgressEvent) int { return a.Time.Compare(b.Time) })

	return all
}
