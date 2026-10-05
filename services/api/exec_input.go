package api

import (
	"context"
	"errors"
	"io"
	"sync"
)

var errExecInputFull = errors.New("the exec input buffer is full")
var errExecInputClosed = errors.New("the exec input is closed")

type execInput struct {
	ctx     context.Context
	mu      sync.Mutex
	pending []byte
	writing int
	ended   bool
	changed chan struct{}
}

func newExecInput(ctx context.Context) *execInput {
	return &execInput{ctx: ctx, changed: make(chan struct{}, 1)}
}

func (q *execInput) offer(data []byte) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.ended {
		return errExecInputClosed
	}
	if len(data) > MaxPayload-q.writing-len(q.pending) {
		return errExecInputFull
	}
	q.pending = append(q.pending, data...)
	q.wake()
	return nil
}

func (q *execInput) end() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.ended = true
	q.wake()
}

func (q *execInput) wake() {
	select {
	case q.changed <- struct{}{}:
	default:
	}
}

func (q *execInput) Read(p []byte) (int, error) {
	q.mu.Lock()
	// The next read proves the copier wrote all bytes from the previous read.
	q.writing = 0
	q.mu.Unlock()
	for {
		if err := q.ctx.Err(); err != nil {
			return 0, err
		}
		q.mu.Lock()
		if len(q.pending) != 0 {
			n := copy(p, q.pending)
			q.pending = q.pending[n:]
			q.writing = n
			q.mu.Unlock()
			return n, nil
		}
		ended := q.ended
		q.mu.Unlock()
		if ended {
			return 0, io.EOF
		}
		select {
		case <-q.ctx.Done():
			return 0, q.ctx.Err()
		case <-q.changed:
		}
	}
}
