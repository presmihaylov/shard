// Package kmsg reads the kernel log ring, where netfilter log rules write with no daemon in between.
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

// parse reads one /dev/kmsg record, "prio,seq,usec_since_boot,flags;message", against the boot time.
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

	// A record may carry a dictionary after the first line; the message is the first line alone.
	message, _, _ = strings.Cut(message, "\n")

	return Line{Time: boot.Add(time.Duration(usec) * time.Microsecond), Message: message}, nil
}
