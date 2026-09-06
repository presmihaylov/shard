package egress

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/kmsg"
	"github.com/presmihaylov/shard/services/network"
)

// Ring is the kernel ring buffer the host chains log their drops into.
type Ring interface {
	Read() ([]kmsg.Record, error)
}

// LogReader is both halves of what one sandbox's egress produced: the proxy's own records, and the
// packets the host chains dropped before the proxy ever saw them.
type LogReader struct {
	log  *Log
	ring Ring
}

func NewLogReader(log *Log, ring Ring) *LogReader { return &LogReader{log: log, ring: ring} }

// Read merges both halves by time, oldest first. The ring is shared and short, so the host's oldest
// lines fall off it while the sandbox's own file keeps every line the proxy wrote.
func (r *LogReader) Read(sb models.Sandbox) ([]Record, error) {
	decisions, err := r.log.Read(sb.ID)
	if err != nil {
		return nil, err
	}

	lines, err := r.ring.Read()
	if err != nil {
		return nil, fmt.Errorf("read the kernel ring buffer: %w", err)
	}

	return Merge(decisions, HostDrops(lines, sb)), nil
}

// HostDrops keeps the ring's lines that this sandbox's chains wrote. The kernel names the bridge and never
// the port, so the source address is what picks the sandbox, and an address is reused: a line older than
// the sandbox belongs to whoever held the address before it.
func HostDrops(lines []kmsg.Record, sb models.Sandbox) []Record {
	if !sb.Address.IsValid() {
		return nil
	}
	source := sb.Address.Addr().String()

	var records []Record
	for _, line := range lines {
		record, ok := hostDrop(line.Message)
		if !ok || record.source != source || line.Time.Before(sb.CreatedAt) {
			continue
		}

		record.Time = line.Time
		records = append(records, record.Record)
	}

	return records
}

// drop is one parsed log line: the record it becomes, and the address that says whose it is.
type drop struct {
	Record
	source string
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
			Reason:  "the host chains dropped a " + protocol(fields["PROTO"]) + " packet",
		},
		source: fields["SRC"],
	}, true
}

func protocol(proto string) string {
	if proto == "" {
		return "raw"
	}

	return strings.ToLower(proto)
}
