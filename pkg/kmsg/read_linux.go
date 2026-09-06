//go:build linux

package kmsg

import (
	"context"
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
	// remarkInterval keeps the drift between the kernel clock and wall time under a second.
	remarkInterval = time.Hour
)

// Follower walks the ring from its oldest record and then stays at its end. It dates every record
// against a mark it wrote itself, never against the boot time, and re-marks so the drift stays small.
type Follower struct {
	fd         int
	markUptime time.Duration
	markWall   time.Time
	marked     time.Time
	// overwritten counts the records the kernel dropped under the reader, which no reader can get back.
	overwritten uint64
}

// Overwritten is how many records the ring lost under this follower.
func (f *Follower) Overwritten() uint64 { return f.overwritten }

// Open opens the ring at its oldest record and writes the first mark.
func Open() (*Follower, error) {
	fd, err := openRing()
	if err != nil {
		return nil, err
	}

	f := &Follower{fd: fd}
	if err := f.mark(); err != nil {
		unix.Close(fd)

		return nil, err
	}

	return f, nil
}

func (f *Follower) Close() error {
	if err := unix.Close(f.fd); err != nil {
		return fmt.Errorf("close %s: %w", device, err)
	}

	return nil
}

// Follow hands every record to yield, oldest first, until ctx ends or yield fails. caughtUp is called
// once, when the backlog the ring held at Open is spent and the follower sits at its end.
func (f *Follower) Follow(ctx context.Context, yield func(Record) error, caughtUp func()) error {
	buf := make([]byte, MaxLine)
	var announced bool

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Since(f.marked) >= remarkInterval {
			if err := f.mark(); err != nil {
				return err
			}
		}

		n, err := unix.Read(f.fd, buf)
		if errors.Is(err, unix.EAGAIN) {
			if !announced {
				announced = true
				caughtUp()
			}

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(markPoll):
			}

			continue
		}
		// EPIPE says the record this read wanted was overwritten; the kernel has already moved to the next one.
		if errors.Is(err, unix.EPIPE) {
			f.overwritten++

			continue
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", device, err)
		}

		record, err := parse(string(buf[:n]))
		if err != nil {
			return err
		}
		if err := yield(at(record, f.markUptime, f.markWall)); err != nil {
			return err
		}
	}
}

// mark writes one line into the ring and learns the kernel timestamp it went in at, which is the one
// point where the kernel clock and wall time are both known.
func (f *Follower) mark() error {
	// The watcher is opened before the mark is written and sits at the end of the ring, so it reads the mark and nothing older.
	watcher, err := openRing()
	if err != nil {
		return err
	}
	defer unix.Close(watcher)

	if _, err := unix.Seek(watcher, 0, unix.SEEK_END); err != nil {
		return fmt.Errorf("seek to the end of %s: %w", device, err)
	}

	mark, wall, err := writeMark()
	if err != nil {
		return err
	}

	uptime, err := awaitMark(watcher, mark)
	if err != nil {
		return err
	}

	f.markUptime, f.markWall, f.marked = uptime, wall, time.Now()

	return nil
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

func openRing() (int, error) {
	fd, err := unix.Open(device, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", device, err)
	}

	return fd, nil
}
