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
		if err := pair.Close(); err != nil {
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
	wrote := make(chan error, 1)
	go func() {
		_, err := pair.Master.Write(bytes.Repeat([]byte("x"), 1<<20))
		wrote <- err
	}()
	select {
	case err := <-wrote:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("the full terminal returned %v, want a write deadline", err)
		}
	case <-time.After(time.Second):
		t.Fatal("the full terminal ignored its write deadline")
	}
	if err := pair.Master.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	read := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(pair.Replica, make([]byte, 3))
		read <- err
	}()
	if _, err := pair.Master.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := <-read; err != nil {
		t.Fatal(err)
	}
}
