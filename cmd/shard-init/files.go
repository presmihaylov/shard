package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"

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

	stat, err := serveFile(conn, header)
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
	if header.Op != supervisor.OpGet {
		return
	}
	if err := sendFile(conn, header.Path, stat.Size); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init:", err)
	}
}

func serveFile(conn net.Conn, header supervisor.FileHeader) (supervisor.FileStat, error) {
	if !filepath.IsAbs(header.Path) {
		return supervisor.FileStat{}, fmt.Errorf("a guest path must be absolute, got %q", header.Path)
	}

	switch header.Op {
	case supervisor.OpStat:
		return statFile(header.Path)
	case supervisor.OpGet:
		stat, err := statFile(header.Path)
		if err != nil {
			return supervisor.FileStat{}, err
		}
		if stat.Dir {
			return supervisor.FileStat{}, fmt.Errorf("%s is a directory; a get takes one file", header.Path)
		}

		return stat, nil
	case supervisor.OpPut:
		if err := receiveFile(conn, header); err != nil {
			return supervisor.FileStat{}, err
		}

		return statFile(header.Path)
	default:
		return supervisor.FileStat{}, fmt.Errorf("the host asked for %q, which the guest does not take", header.Op)
	}
}

func statFile(path string) (supervisor.FileStat, error) {
	info, err := os.Stat(path)
	if err != nil {
		return supervisor.FileStat{}, err
	}

	return supervisor.FileStat{Name: info.Name(), Size: info.Size(), Mode: uint32(info.Mode()), ModTime: info.ModTime(), Dir: info.IsDir()}, nil
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

// sendFile streams exactly the size the reply promised; a file that changed meanwhile ends the stream short, which the host reports.
func sendFile(conn net.Conn, path string, size int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := io.CopyN(conn, f, size); err != nil {
		return fmt.Errorf("send %s: %w", path, err)
	}

	return nil
}
