//go:build linux

package kmsg

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// markPrefix starts the line this package writes into the ring to date every other line by. It carries a
// nonce, so a mark of an earlier read is never mistaken for this one.
const markPrefix = "shard-kmsg-mark "

// device is the ring. It is opened raw, never as an *os.File: an *os.File joins the Go poller, and a
// non-blocking read that the poller waits on hangs at the end of the ring instead of answering EAGAIN.
const device = "/dev/kmsg"

// Read returns every record the ring still holds, oldest first, dated against a mark written now.
func Read() ([]Record, error) {
	mark, wall, err := writeMark()
	if err != nil {
		return nil, err
	}

	records, err := readAll()
	if err != nil {
		return nil, err
	}

	uptime, found := findMark(records, mark)
	if !found {
		return nil, fmt.Errorf("the mark this read wrote is not in %s: the ring turned over mid-read", device)
	}

	return date(records, uptime, wall), nil
}

// writeMark puts one debug line into the ring and answers with the wall time it went in at. The kernel
// dates it on the same clock as every other record, so it is the one point where both clocks are known.
func writeMark() (string, time.Time, error) {
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return "", time.Time{}, fmt.Errorf("draw a mark nonce: %w", err)
	}
	mark := markPrefix + hex.EncodeToString(nonce)

	file, err := os.OpenFile(device, os.O_WRONLY, 0)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("open %s to write: %w", device, err)
	}
	defer file.Close()

	wall := time.Now().UTC()
	// <7> is KERN_DEBUG, so the mark is the quietest thing that can go in.
	if _, err := file.WriteString("<7>" + mark); err != nil {
		return "", time.Time{}, fmt.Errorf("write the mark to %s: %w", device, err)
	}

	return mark, wall, nil
}

func findMark(records []Record, mark string) (time.Duration, bool) {
	for i := len(records) - 1; i >= 0; i-- {
		if strings.Contains(records[i].Message, mark) {
			return records[i].Uptime, true
		}
	}

	return 0, false
}

// readAll walks the ring from its oldest record to its end. One read is one record, and the buffer is
// sized to MaxLine, so a record longer than that is truncated by the kernel rather than split over reads.
func readAll() ([]Record, error) {
	fd, err := unix.Open(device, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", device, err)
	}
	defer unix.Close(fd)

	var records []Record
	buf := make([]byte, MaxLine)

	for {
		n, err := unix.Read(fd, buf)
		if errors.Is(err, unix.EAGAIN) {
			return records, nil
		}
		// EPIPE says the record this read wanted was overwritten; the kernel has already moved to the next one.
		if errors.Is(err, unix.EPIPE) {
			continue
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", device, err)
		}

		record, err := parse(string(buf[:n]))
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
}
