// Package kmsg reads the kernel log ring, where netfilter log rules write with no daemon in between.
package kmsg

import (
	"errors"
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

// markPrefix names the records Read writes to anchor the kernel clock; at debug priority, so nothing prints them.
const markPrefix = "shard-kmsg-mark "

// record is one /dev/kmsg record before its clock is anchored to wall time.
type record struct {
	sinceBoot time.Duration
	message   string
}

// parse reads one /dev/kmsg record, "prio,seq,usec_since_boot,flags;message".
func parse(raw string) (record, error) {
	header, message, ok := strings.Cut(raw, ";")
	if !ok {
		return record{}, fmt.Errorf("kmsg record has no message: %q", raw)
	}

	fields := strings.Split(header, ",")
	if len(fields) < 3 {
		return record{}, fmt.Errorf("kmsg record has %d header fields, want at least 3: %q", len(fields), raw)
	}

	usec, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return record{}, fmt.Errorf("kmsg record timestamp: %w", err)
	}

	// A record may carry a dictionary after the first line; the message is the first line alone.
	message, _, _ = strings.Cut(message, "\n")

	return record{sinceBoot: time.Duration(usec) * time.Microsecond, message: message}, nil
}

// lines dates every record against the mark written at now and drops the marks; the kernel clock drifts from boot time on a long-up host.
func lines(records []record, mark string, now time.Time) ([]Line, error) {
	boot, err := anchor(records, mark, now)
	if err != nil {
		return nil, err
	}

	out := make([]Line, 0, len(records))
	for _, r := range records {
		if strings.HasPrefix(r.message, markPrefix) {
			continue
		}
		out = append(out, Line{Time: boot.Add(r.sinceBoot), Message: r.message})
	}
	return out, nil
}

// anchor is the wall time of the kernel clock's zero, from the mark written at now.
func anchor(records []record, mark string, now time.Time) (time.Time, error) {
	for _, r := range records {
		if r.message == mark {
			return now.Add(-r.sinceBoot), nil
		}
	}
	return time.Time{}, errors.New("the kernel log dropped the mark before it was read")
}
