package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

func TestExecInputCountsTheCopyAndPreservesOrderThroughEOF(t *testing.T) {
	q := newExecInput(t.Context())
	first := bytes.Repeat([]byte("a"), MaxPayload/2)
	second := bytes.Repeat([]byte("b"), MaxPayload/2)
	if err := q.offer(first); err != nil {
		t.Fatal("the first half was refused")
	}
	out := make([]byte, MaxPayload)
	n, err := q.Read(out)
	if err != nil || !bytes.Equal(out[:n], first) {
		t.Fatalf("the first read: %d, %v", n, err)
	}
	if err := q.offer(second); err != nil {
		t.Fatal("the second half was refused")
	}
	if err := q.offer([]byte("x")); !errors.Is(err, errExecInputFull) {
		t.Fatal("input held by the copier did not count toward the limit")
	}
	q.end()
	n, err = q.Read(out)
	if err != nil || !bytes.Equal(out[:n], second) {
		t.Fatalf("the second read lost order: %d, %v", n, err)
	}
	n, err = q.Read(out)
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("EOF did not follow all input: %d, %v", n, err)
	}
}

func TestExecInputCancelUnblocksAnEmptyRead(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	q := newExecInput(ctx)
	read := make(chan error, 1)
	go func() {
		_, err := q.Read(make([]byte, 1))
		read <- err
	}()
	cancel()
	if err := <-read; !errors.Is(err, context.Canceled) {
		t.Fatalf("the cancelled read returned %v", err)
	}
}
