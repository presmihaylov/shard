package egress

import (
	"strconv"
	"strings"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/network"
)

// LogReader is what shard logs --egress reads: one sandbox's own file. The daemon writes both halves
// into it, the proxy's own decisions and the packets the host chains dropped, so a read needs no ring.
type LogReader struct {
	log *Log
}

func NewLogReader(log *Log) *LogReader { return &LogReader{log: log} }

// Read returns the sandbox's records by time, oldest first. The file holds each source in time order
// already, and the tailer appends a host drop within a second of it, so a sort is enough.
func (r *LogReader) Read(sb models.Sandbox) ([]Record, error) {
	records, err := r.log.Read(sb.ID)
	if err != nil {
		return nil, err
	}

	return Merge(records), nil
}

// drop is one parsed log line: the record it becomes, and the two things that say whose it is.
type drop struct {
	Record
	source string
	iface  string
}

func hostDrop(message string) (drop, bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(message), network.LogPrefix+" ")
	if !found {
		return drop{}, false
	}

	fields := map[string]string{}
	for field := range strings.FieldsSeq(rest) {
		key, value, ok := strings.Cut(field, "=")
		if ok {
			fields[key] = value
		}
	}

	rule, ok := fields["rule"]
	if !ok {
		return drop{}, false
	}

	port, _ := strconv.Atoi(fields["DPT"])

	return drop{
		Record: Record{
			Source:  SourceHost,
			Verdict: string(models.ActionDeny),
			Port:    port,
			Address: fields["DST"],
			Rule:    rule,
			Reason:  reason(rule, protocol(fields["PROTO"])),
		},
		source: fields["SRC"],
		iface:  fields["IN"],
	}, true
}

// reason says what the chain that logged the line was doing, because neither a local drop nor an IPv6
// one is a policy drop.
func reason(rule, proto string) string {
	if rule == network.RuleLocal {
		return "the host chains dropped a " + proto + " packet to the host's own address"
	}
	if rule == network.RuleIPv6 {
		return "the host chains dropped an IPv6 " + proto + " packet: no policy rule can match one"
	}

	return "the host chains dropped a " + proto + " packet"
}

func protocol(proto string) string {
	if proto == "" {
		return "raw"
	}

	return strings.ToLower(proto)
}
