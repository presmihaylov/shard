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

// markWait bounds how long the mark is waited for. A write to the ring is committed by the kernel's own
// deferred work, so a read that follows it at once does not see it yet.
const (
	markWait = 2 * time.Second
	markPoll = 10 * time.Millisecond
)

// Read returns every record the ring still holds, oldest first, dated against a mark written now.
func Read() ([]Record, error) {
	// The watcher is opened before the mark is written and sits at the end of the ring, so it reads the mark and nothing older.
	watcher, err := openRing()
	if err != nil {
		return nil, err
	}
	defer unix.Close(watcher)

	if _, err := unix.Seek(watcher, 0, unix.SEEK_END); err != nil {
		return nil, fmt.Errorf("seek to the end of %s: %w", device, err)
	}

	mark, wall, err := writeMark()
	if err != nil {
		return nil, err
	}

	uptime, err := awaitMark(watcher, mark)
	if err != nil {
		return nil, err
	}

	records, err := readAll()
	if err != nil {
		return nil, err
	}

	return date(records, uptime, wall), nil
}

// awaitMark reads the ring's new records until the mark arrives, and answers with the kernel's own timestamp for it.
func awaitMark(fd int, mark string) (time.Duration, error) {
	buf := make([]byte, MaxLine)

	for deadline := time.Now().Add(markWait); time.Now().Before(deadline); {
		n, err := unix.Read(fd, buf)
		if errors.Is(err, unix.EAGAIN) {
			time.Sleep(markPoll)

			continue
		}
		if errors.Is(err, unix.EPIPE) || errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("read %s: %w", device, err)
		}

		record, err := parse(string(buf[:n]))
		if err != nil {
			return 0, err
		}
		if strings.Contains(record.Message, mark) {
			return record.Uptime, nil
		}
	}

	return 0, fmt.Errorf("the mark this read wrote did not reach %s in %s", device, markWait)
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
	// The mark goes in twice: a reader sees a record only once the next one is reserved, so the second finalizes the first.
	for range 2 {
		// <7> is KERN_DEBUG, so the mark is the quietest thing that can go in.
		if _, err := file.WriteString("<7>" + mark); err != nil {
			return "", time.Time{}, fmt.Errorf("write the mark to %s: %w", device, err)
		}
	}

	return mark, wall, nil
}

// readAll walks the ring from its oldest record to its end. One read is one record, and the buffer is
// sized to MaxLine, so a record longer than that is truncated by the kernel rather than split over reads.
func readAll() ([]Record, error) {
	fd, err := openRing()
	if err != nil {
		return nil, err
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

func openRing() (int, error) {
	fd, err := unix.Open(device, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", device, err)
	}

	return fd, nil
}
