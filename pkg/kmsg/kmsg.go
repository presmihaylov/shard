// Package kmsg reads the kernel ring buffer. It is a driver over /dev/kmsg and knows nothing of what
// the lines mean: the caller picks out the ones it wrote.
package kmsg

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// MaxLine is the longest record read. A record longer than this is dropped rather than split.
const MaxLine = 64 << 10

// Record is one line of the ring. Time is wall time, dated against a mark this package wrote, never
// against the boot time: the kernel's monotonic clock drifts from btime by tens of seconds in a fortnight.
type Record struct {
	Priority int
	Sequence uint64
	// Uptime is the kernel's own timestamp, the offset from boot the record carries.
	Uptime  time.Duration
	Time    time.Time
	Message string
}

// ErrNotSupported is what every verb returns off Linux.
var ErrNotSupported = errors.New("the kernel ring buffer is a Linux interface")

// parse reads one record of the ring. A record with a dictionary keeps its first line only: the
// dictionary repeats what the message already says and no caller here reads it.
func parse(raw string) (Record, error) {
	head, message, found := strings.Cut(raw, ";")
	if !found {
		return Record{}, fmt.Errorf("the record %q carries no ;", clip(raw))
	}

	message, _, _ = strings.Cut(message, "\n")

	fields := strings.Split(head, ",")
	if len(fields) < 3 {
		return Record{}, fmt.Errorf("the record head %q is not priority,sequence,timestamp", clip(head))
	}

	priority, err := strconv.Atoi(fields[0])
	if err != nil {
		return Record{}, fmt.Errorf("the priority %q is not a number", fields[0])
	}

	sequence, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return Record{}, fmt.Errorf("the sequence %q is not a number", fields[1])
	}

	micros, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return Record{}, fmt.Errorf("the timestamp %q is not a number", fields[2])
	}

	return Record{
		Priority: priority,
		Sequence: sequence,
		Uptime:   time.Duration(micros) * time.Microsecond,
		Message:  message,
	}, nil
}

func clip(text string) string {
	const most = 64
	if len(text) <= most {
		return text
	}

	return text[:most] + "..."
}

// date turns every record's uptime into wall time, against one mark whose uptime and wall time are both known.
func date(records []Record, markUptime time.Duration, markWall time.Time) []Record {
	dated := make([]Record, 0, len(records))
	for _, record := range records {
		record.Time = markWall.Add(record.Uptime - markUptime).UTC()
		dated = append(dated, record)
	}

	return dated
}

// Reader is the ring itself. It holds nothing: every Read walks the ring from its oldest record.
type Reader struct{}

func (Reader) Read() ([]Record, error) { return Read() }
