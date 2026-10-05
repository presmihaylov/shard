//go:build linux || darwin

package pty

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestMasterWriteDeadlineSurvivesResizeAndCanBeCleared(t *testing.T) {
	pair, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := errors.Join(pair.Master.Close(), pair.Replica.Close()); err != nil {
			t.Error(err)
		}
	}()
	flags, err := unix.FcntlInt(pair.Master.Fd(), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("the terminal master can leak into a child: flags %d, %v", flags, err)
	}
	restore, err := MakeRaw(pair.Replica)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := restore(); err != nil {
			t.Error(err)
		}
	}()
	if err := pair.Resize(Size{Rows: 24, Cols: 80}); err != nil {
		t.Fatal(err)
	}
	if err := pair.Master.SetWriteDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	type writeResult struct {
		n   int
		err error
	}
	wrote := make(chan writeResult, 1)
	go func() {
		n, err := pair.Master.Write(bytes.Repeat([]byte("x"), 1<<20))
		wrote <- writeResult{n: n, err: err}
	}()
	var pending int
	select {
	case result := <-wrote:
		if !errors.Is(result.err, os.ErrDeadlineExceeded) {
			t.Fatalf("the full terminal returned %v, want a write deadline", result.err)
		}
		pending = result.n
	case <-time.After(time.Second):
		t.Fatal("the full terminal ignored its write deadline")
	}
	if err := pair.Master.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	next := []byte("new")
	data := make([]byte, pending+len(next))
	read := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(pair.Replica, data)
		read <- err
	}()
	go func() {
		n, err := pair.Master.Write(next)
		wrote <- writeResult{n: n, err: err}
	}()
	select {
	case result := <-wrote:
		if result.err != nil || result.n != len(next) {
			t.Fatalf("write after the deadline was cleared: %d bytes, %v", result.n, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("the cleared deadline kept the next write blocked")
	}
	select {
	case err := <-read:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("the replica did not receive the accepted input")
	}
	if !bytes.Equal(data[pending:], next) {
		t.Fatalf("read %q after the old input, want %q", data[pending:], next)
	}
}
