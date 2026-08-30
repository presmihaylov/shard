// Package kmsg reads the kernel log ring. Netfilter log rules write there, and nothing else on the
// host has to run for a line to land, so a reader can open it after the fact.
package kmsg

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Line is one kernel log record with its timestamp turned into wall time.
type Line struct {
	Time    time.Time
	Message string
}

// parse reads one /dev/kmsg record: "prio,seq,usec_since_boot,flags;message". boot is when the clock started.
func parse(record string, boot time.Time) (Line, error) {
	header, message, ok := strings.Cut(record, ";")
	if !ok {
		return Line{}, fmt.Errorf("kmsg record has no message: %q", record)
	}

	fields := strings.Split(header, ",")
	if len(fields) < 3 {
		return Line{}, fmt.Errorf("kmsg record has %d header fields, want at least 3: %q", len(fields), record)
	}

	usec, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return Line{}, fmt.Errorf("kmsg record timestamp: %w", err)
	}

	return Line{Time: boot.Add(time.Duration(usec) * time.Microsecond), Message: strings.TrimSuffix(message, "\n")}, nil
}
