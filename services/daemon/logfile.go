package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"syscall"
	"time"
)

// logCap is the size past which a Mac daemon moves its log aside itself, between two newsyslog runs.
const logCap = 64 << 20

// LogOverflow is the suffix of the one file a capped log moves to; newsyslog's own archives end in .0 to .6.
const LogOverflow = ".overflow"

// logCapInterval bounds how far past the cap the log grows before the daemon sees it.
const logCapInterval = time.Second

// logReopen reopens the daemon's log on each SIGHUP, so newsyslog can rotate it on a Mac, and moves it aside past its cap.
type logReopen struct {
	path     string
	limit    int64
	interval time.Duration
	hangups  chan os.Signal
	out      io.Writer
	reopen   func(path string) error
}

func (logReopen) Name() string { return "log-reopen" }

func (t logReopen) Run(ctx context.Context) error {
	tick := time.NewTicker(t.interval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.hangups:
			if err := t.reopenLog(); err != nil {
				return err
			}
			log.New(t.out, "", log.LstdFlags).Printf("daemon reopened %s on SIGHUP", t.path)
		case <-tick.C:
			if err := t.capLog(); err != nil {
				return err
			}
		}
	}
}

// capLog moves a log past the cap to its one overflow file, replacing the one before, and reopens the path.
func (t logReopen) capLog() error {
	info, err := os.Stat(t.path)
	// newsyslog moved it aside, and its SIGHUP reopens it.
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("measure the log %s: %w", t.path, err)
	}
	if info.Size() < t.limit {
		return nil
	}

	if err := os.Rename(t.path, t.path+LogOverflow); err != nil {
		return fmt.Errorf("move the log %s aside past its cap: %w", t.path, err)
	}
	if err := t.reopenLog(); err != nil {
		return err
	}
	log.New(t.out, "", log.LstdFlags).Printf("daemon moved %s to %s at %d bytes, past its cap of %d", t.path, t.path+LogOverflow, info.Size(), t.limit)

	return nil
}

func (t logReopen) reopenLog() error {
	if err := t.reopen(t.path); err != nil {
		// Put a hangup back, so the restart retries after its backoff and not at a rotation that never comes.
		select {
		case t.hangups <- syscall.SIGHUP:
		default:
		}

		return err
	}

	return nil
}
