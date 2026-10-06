package egress

import (
	"strconv"
	"strings"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/network"
)

// LogReader is what shard policy logs reads: one sandbox's own file. The daemon writes both halves
// into it, the proxy's own decisions and the packets the host chains dropped, so a read needs no ring.
type LogReader struct {
	log *Log
}

func NewLogReader(log *Log) *LogReader { return &LogReader{log: log} }

// Read returns the sandbox's newest records by time, oldest first, and how many older ones it left out. Each source is in time order in the file, and a host drop lands within a second, so a sort is enough.
func (r *LogReader) Read(sb models.Sandbox) ([]Record, int, error) {
	records, cut, err := r.log.Tail(sb.ID)
	if err != nil {
		return nil, 0, err
	}

	return Merge(records), cut, nil
}

// drop is one parsed log line: the record it becomes, the two things that say whose it is, and the sum of the rule that wrote it.
type drop struct {
	Record
	source string
	iface  string
	sum    string
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
		sum:    fields["sum"],
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
