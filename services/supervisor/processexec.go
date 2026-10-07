package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/presmihaylov/shard/models"
)

// processBound bounds a run's exec, which the guest answers once the process forked; a stop-process waits its grace on top.
const processBound = 30 * time.Second

// RunProcess starts one named process through PID 1 of a sandbox the host execs into, as Control.Run does on a VM; a Refusal says why not.
func RunProcess(ctx context.Context, run ExecFunc, spec RunSpec) error {
	return requestProcess(ctx, run, Message{Kind: KindRun, Run: &spec}, processBound)
}

// StopProcess terms one named process through that PID 1, kills it once grace passes, and returns once the guest reaped it.
func StopProcess(ctx context.Context, run ExecFunc, name string, grace time.Duration) error {
	return requestProcess(ctx, run, Message{Kind: KindStopProcess, Name: name, Grace: grace}, grace+processBound)
}

// requestProcess hands m to `/.shard/init process` as root and reads its one answer; the provider's exec kills a guest that outlives bound.
func requestProcess(ctx context.Context, run ExecFunc, m Message, bound time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()

	pipes, err := openPipes()
	if err != nil {
		return err
	}
	stdin, stdout, stderr := pipes[0], pipes[1], pipes[2]
	// A guest process that keeps a pipe open would hold a read or the write for good, so each ends at the bound too.
	deadline, _ := ctx.Deadline()
	if err := errors.Join(stdin.w.SetWriteDeadline(deadline), stdout.r.SetReadDeadline(deadline), stderr.r.SetReadDeadline(deadline)); err != nil {
		return errors.Join(fmt.Errorf("bound the process exec: %w", err), closePipes(pipes[:]))
	}

	sent := make(chan error, 1)
	go func() { sent <- errors.Join(WriteMessage(stdin.w, m), stdin.w.Close()) }()
	var answer []byte
	var tail string
	var answerErr, tailErr error
	var reads sync.WaitGroup
	reads.Go(func() { answer, answerErr = readAnswer(stdout.r) })
	reads.Go(func() { tail, tailErr = readTail(stderr.r) })

	spec := models.ExecSpec{Argv: []string{InitPath, ProcessMode}, User: "0:0", WorkDir: "/", Stdin: stdin.r, Stdout: stdout.w, Stderr: stderr.w}
	exit, runErr := run(ctx, spec)
	// Our copies of the guest's ends keep each pipe open, so the write meets EPIPE and the reads EOF only once they go.
	closeErr := errors.Join(stdin.r.Close(), stdout.w.Close(), stderr.w.Close())
	reads.Wait()
	sendErr := <-sent
	closeErr = errors.Join(closeErr, stdout.r.Close(), stderr.r.Close())

	if len(bytes.TrimSpace(answer)) > 0 {
		if err := errors.Join(answerErr, tailErr, sendErr, closeErr); err != nil {
			return fmt.Errorf("%s %s: %w", InitPath, ProcessMode, err)
		}

		return processReply(m.Kind, answer)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%s %s did not answer within %s: %w", InitPath, ProcessMode, bound, errors.Join(ctx.Err(), runErr))
	}
	if runErr != nil {
		return fmt.Errorf("run %s %s: %w", InitPath, ProcessMode, errors.Join(runErr, closeErr))
	}
	if err := errors.Join(answerErr, tailErr, closeErr); err != nil {
		return fmt.Errorf("%s %s: %w", InitPath, ProcessMode, err)
	}
	if exit.Code == 0 && exit.Signal == 0 {
		return errors.Join(fmt.Errorf("%s %s exited 0 with no answer", InitPath, ProcessMode), sendErr)
	}

	// A shard-init from before named processes takes "process" for a bad flag and exits before it reads stdin, so the send's broken pipe says nothing more.
	return &Refusal{Kind: m.Kind, Answer: Message{Kind: KindFailure, Error: OneLine(tail), Outdated: true}}
}

// processReply reads the guest's one answer line: done is nil, and a failure is a Refusal that ProcessError maps.
func processReply(kind string, answer []byte) error {
	var reply Message
	if err := ReadHeader(bytes.NewReader(answer), &reply); err != nil {
		return fmt.Errorf("%s %s answered %q: %w", InitPath, ProcessMode, OneLine(string(answer)), err)
	}
	if reply.Kind == KindFailure {
		return &Refusal{Kind: kind, Answer: reply}
	}
	if reply.Kind != KindDone {
		return fmt.Errorf("%s: the guest answered with %q, not done", kind, reply.Kind)
	}

	return nil
}

// readAnswer keeps at most one line's bound of stdout and drains the rest, so a guest that floods it ends rather than blocks.
func readAnswer(r io.Reader) ([]byte, error) {
	answer, err := io.ReadAll(io.LimitReader(r, MaxPayload+1))
	if err != nil {
		return answer, fmt.Errorf("read the process exec stdout: %w", err)
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		return answer, fmt.Errorf("drain the process exec stdout: %w", err)
	}

	return answer, nil
}
