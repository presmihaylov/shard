package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"

	"github.com/presmihaylov/shard/services/supervisor"
)

// acceptFiles gives every stat or copy its own connection, as an exec has, so one long copy never blocks a stat.
func (t *transport) acceptFiles(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			fmt.Fprintln(os.Stderr, "shard-init: accept a files connection:", err)

			return
		}
		go serveFiles(conn)
	}
}

// serveFiles does the one operation the header names and answers with the file's shape, or with why not.
func serveFiles(conn net.Conn) {
	defer conn.Close()

	var header supervisor.FileHeader
	if err := supervisor.ReadHeader(conn, &header); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init: read a files header:", err)

		return
	}

	stat, src, err := serveFile(conn, header)
	if err != nil {
		if err := supervisor.WriteMessage(conn, supervisor.FileReply{Error: err.Error()}); err != nil {
			fmt.Fprintln(os.Stderr, "shard-init:", err)
		}

		return
	}
	if err := supervisor.WriteMessage(conn, supervisor.FileReply{Stat: &stat}); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init:", err)

		return
	}
	if src == nil {
		return
	}
	defer src.Close()
	if _, err := io.CopyN(conn, src, stat.Size); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init: send", header.Path+":", err)
	}
}

// serveFile does the operation and, for a get, hands back the open file, so what the reply describes is what the bytes come from.
func serveFile(conn net.Conn, header supervisor.FileHeader) (supervisor.FileStat, *os.File, error) {
	if !filepath.IsAbs(header.Path) {
		return supervisor.FileStat{}, nil, fmt.Errorf("a guest path must be absolute, got %q", header.Path)
	}

	switch header.Op {
	case supervisor.OpStat:
		info, err := os.Stat(header.Path)
		if err != nil {
			return supervisor.FileStat{}, nil, err
		}

		return statOf(info), nil, nil
	case supervisor.OpGet:
		return openFile(header.Path)
	case supervisor.OpPut:
		if err := receiveFile(conn, header); err != nil {
			return supervisor.FileStat{}, nil, err
		}
		info, err := os.Stat(header.Path)
		if err != nil {
			return supervisor.FileStat{}, nil, err
		}

		return statOf(info), nil, nil
	default:
		return supervisor.FileStat{}, nil, fmt.Errorf("unknown files op %q", header.Op)
	}
}

// openFile opens a get's source and refuses anything but a regular file: a fifo would block the open, a directory has no bytes.
func openFile(path string) (supervisor.FileStat, *os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return supervisor.FileStat{}, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return supervisor.FileStat{}, nil, errors.Join(err, f.Close())
	}
	if info.IsDir() {
		return supervisor.FileStat{}, nil, errors.Join(fmt.Errorf("%s is a directory; a get takes one file", path), f.Close())
	}
	if !info.Mode().IsRegular() {
		return supervisor.FileStat{}, nil, errors.Join(fmt.Errorf("%s is a %s, not a regular file; a get takes one file", path, info.Mode().Type()), f.Close())
	}

	return statOf(info), f, nil
}

func statOf(info os.FileInfo) supervisor.FileStat {
	return supervisor.FileStat{Name: info.Name(), Size: info.Size(), Mode: uint32(info.Mode()), ModTime: info.ModTime(), Dir: info.IsDir()}
}

// receiveFile takes the host's bytes into a temp name beside the target, so a copy that dies midway leaves the old file whole.
func receiveFile(conn net.Conn, header supervisor.FileHeader) error {
	f, err := os.CreateTemp(filepath.Dir(header.Path), ".shard-put-*")
	if err != nil {
		return err
	}
	if err := fillFile(f, conn, header); err != nil {
		return errors.Join(err, removeTemp(f.Name()))
	}
	if err := os.Rename(f.Name(), header.Path); err != nil {
		return errors.Join(err, removeTemp(f.Name()))
	}

	return nil
}

// fillFile lands the bytes, the mode and the sync, and closes; the rename waits for the close so no host sees a file still being written.
func fillFile(f *os.File, conn net.Conn, header supervisor.FileHeader) error {
	if _, err := io.CopyN(f, conn, header.Size); err != nil {
		return errors.Join(fmt.Errorf("receive %d bytes: %w", header.Size, err), f.Close())
	}
	if err := f.Chmod(fs.FileMode(header.Mode).Perm()); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(err, f.Close())
	}

	return f.Close()
}

func removeTemp(name string) error {
	if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	return nil
}
