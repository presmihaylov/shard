package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/presmihaylov/shard/models"
)

// stderrTail bounds what a failed files exec keeps of its stderr, which is the reason the guest gave.
const stderrTail = 4 << 10

// ExecFunc runs one command in a sandbox with the stdio spec carries, as models.Provider.Exec does.
type ExecFunc func(ctx context.Context, spec models.ExecSpec) (models.ExitStatus, error)

// OpenFiles starts shard-init's files mode through run, as user, and answers its stdio as one connection; Close says how the exec ended.
func OpenFiles(ctx context.Context, run ExecFunc, user string) (FilesConn, error) {
	pipes, err := openPipes()
	if err != nil {
		return nil, err
	}
	stdin, stdout, stderr := pipes[0], pipes[1], pipes[2]

	c := &filesConn{stdin: stdin.w, stdout: stdout.r, done: make(chan struct{})}
	tail := make(chan tailResult, 1)
	go func() {
		text, err := readTail(stderr.r)
		tail <- tailResult{text: text, err: errors.Join(err, stderr.r.Close())}
	}()
	go func() {
		spec := models.ExecSpec{Argv: []string{InitPath, FilesMode}, User: user, WorkDir: "/", Stdin: stdin.r, Stdout: stdout.w, Stderr: stderr.w}
		exit, err := run(ctx, spec)
		// Our copies of the guest's ends keep each pipe open, so a write meets EPIPE and a read EOF only once they go.
		closeErr := errors.Join(stdin.r.Close(), stdout.w.Close(), stderr.w.Close())
		reason := <-tail
		c.exit, c.tail, c.err = exit, reason.text, errors.Join(err, closeErr, reason.err)
		close(c.done)
	}()
	c.stop = context.AfterFunc(ctx, c.shut)

	return c, nil
}

type pipe struct{ r, w *os.File }

// openPipes opens the exec's stdin, stdout and stderr, in that order.
func openPipes() ([3]pipe, error) {
	var pipes [3]pipe
	for i := range pipes {
		r, w, err := os.Pipe()
		if err != nil {
			return pipes, errors.Join(fmt.Errorf("open a pipe for the files exec: %w", err), closePipes(pipes[:i]))
		}
		pipes[i] = pipe{r: r, w: w}
	}

	return pipes, nil
}

func closePipes(pipes []pipe) error {
	var errs []error
	for _, p := range pipes {
		errs = append(errs, p.r.Close(), p.w.Close())
	}

	return errors.Join(errs...)
}

type tailResult struct {
	text string
	err  error
}

// readTail reads r to its end and keeps the last stderrTail bytes, so a guest that floods stderr costs no memory.
func readTail(r io.Reader) (string, error) {
	var kept []byte
	chunk := make([]byte, stderrTail)
	for {
		n, err := r.Read(chunk)
		kept = append(kept, chunk[:n]...)
		if len(kept) > stderrTail {
			kept = kept[len(kept)-stderrTail:]
		}
		if errors.Is(err, io.EOF) {
			return string(kept), nil
		}
		if err != nil {
			return string(kept), fmt.Errorf("read the files exec stderr: %w", err)
		}
	}
}

// filesConn is the host's end of one files exec: its writes are the guest's stdin, its reads the guest's stdout.
type filesConn struct {
	stdin   *os.File
	stdout  *os.File
	done    chan struct{}
	exit    models.ExitStatus
	tail    string
	err     error
	stop    func() bool
	once    sync.Once
	shutErr error
	// stdinOnce lets CloseWrite and shut both close stdin, whichever comes first.
	stdinOnce sync.Once
	stdinErr  error
}

func (c *filesConn) Write(p []byte) (int, error) {
	return c.stdin.Write(p)
}

// Read ends with how the exec ended, so a guest that died before its answer reads as its own reason, not a bare EOF.
func (c *filesConn) Read(p []byte) (int, error) {
	n, err := c.stdout.Read(p)
	if !errors.Is(err, io.EOF) {
		return n, err
	}

	<-c.done
	if failed := c.result(); failed != nil {
		return n, failed
	}

	return n, io.EOF
}

// Close shuts both ends, which unblocks the guest whichever way it waits, and answers once the exec has ended.
func (c *filesConn) Close() error {
	c.stop()
	c.shut()
	<-c.done

	return errors.Join(c.shutErr, c.result())
}

// CloseWrite gives the guest EOF on its stdin and leaves its stdout open for the answer.
func (c *filesConn) CloseWrite() error {
	c.stdinOnce.Do(func() {
		c.stdinErr = c.stdin.Close()
	})

	return c.stdinErr
}

func (c *filesConn) shut() {
	c.once.Do(func() {
		c.shutErr = errors.Join(c.CloseWrite(), c.stdout.Close())
	})
}

func (c *filesConn) result() error {
	reason := strings.TrimSpace(c.tail)
	if c.err != nil {
		return fmt.Errorf("run %s %s: %w", InitPath, FilesMode, c.err)
	}
	if c.exit.Signal != 0 {
		return fmt.Errorf("%s %s ended by signal %d: %s", InitPath, FilesMode, c.exit.Signal, reason)
	}
	if c.exit.Code != 0 {
		return fmt.Errorf("%s %s exited %d: %s", InitPath, FilesMode, c.exit.Code, reason)
	}

	return nil
}
