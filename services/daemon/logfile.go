package daemon

import (
	"context"
	"io"
	"log"
	"os"
	"syscall"
)

// logReopen points the daemon's output at a fresh file on each SIGHUP, so newsyslog can rename the log aside on a Mac.
type logReopen struct {
	path    string
	hangups chan os.Signal
	out     io.Writer
	reopen  func(path string) error
}

func (logReopen) Name() string { return "log-reopen" }

func (t logReopen) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.hangups:
		}

		if err := t.reopen(t.path); err != nil {
			// Put the hangup back, so the restart retries after its backoff and not at a rotation that never comes.
			select {
			case t.hangups <- syscall.SIGHUP:
			default:
			}

			return err
		}
		log.New(t.out, "", log.LstdFlags).Printf("daemon reopened %s on SIGHUP", t.path)
	}
}
