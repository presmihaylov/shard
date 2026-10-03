package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/supervisor"
)

// selfBinary is what a VM runs for supervisor.InitPath, since its shard-init is the initrd's /init and no disk holds it.
var selfBinary = "/proc/self/exe"

// runFiles is the files mode main runs, exit code included: 0 once the host has its answer, 1 when the wire broke.
func runFiles() int {
	if err := filesAsUser(); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init:", err)

		return 1
	}

	return 0
}

// filesAsUser sheds what a runtime handed a non-root user, so the guest kernel checks that user alone: a VM raises PID 1's set into the ambient one.
func filesAsUser() error {
	if os.Geteuid() != 0 {
		if err := dropCapabilities(); err != nil {
			return err
		}
	}

	return serveFiles(os.Stdin, os.Stdout)
}

// serveFiles does the one operation the header names, as the user the exec runs as, and answers with the path's stat or with why not.
func serveFiles(r io.Reader, w io.Writer) error {
	var header supervisor.FileHeader
	if err := supervisor.ReadHeader(r, &header); err != nil {
		return fmt.Errorf("read a files header: %w", err)
	}

	stat, src, err := serveFile(r, header)
	if err != nil {
		return supervisor.WriteMessage(w, supervisor.FileReply{Error: err.Error(), Code: codeOf(err)})
	}
	if err := supervisor.WriteMessage(w, supervisor.FileReply{Stat: &stat}); err != nil {
		return errors.Join(err, closeSource(src))
	}
	if src == nil {
		return nil
	}
	// Plain reads to EOF: a /proc or sysfs file's size is not its length, and a zero-copy path would trust it.
	if _, err := io.Copy(struct{ io.Writer }{w}, struct{ io.Reader }{src}); err != nil {
		return errors.Join(fmt.Errorf("send %s: %w", header.Path, err), src.Close())
	}

	return src.Close()
}

// serveFile does the operation and, for a get, hands back the open file, so what the reply describes is what the bytes come from.
func serveFile(r io.Reader, header supervisor.FileHeader) (models.FileStat, *os.File, error) {
	if !filepath.IsAbs(header.Path) {
		return models.FileStat{}, nil, invalidError(fmt.Sprintf("a guest path must be absolute, got %q", header.Path))
	}

	switch header.Op {
	case supervisor.OpStat:
		info, err := os.Lstat(header.Path)
		if err != nil {
			return models.FileStat{}, nil, err
		}

		return statOf(info), nil, nil
	case supervisor.OpGet:
		return openFile(header.Path)
	case supervisor.OpPut:
		if err := receiveFile(r, header); err != nil {
			return models.FileStat{}, nil, err
		}
		info, err := os.Lstat(header.Path)
		if err != nil {
			return models.FileStat{}, nil, err
		}

		return statOf(info), nil, nil
	default:
		return models.FileStat{}, nil, invalidError(fmt.Sprintf("unknown files op %q", header.Op))
	}
}

// invalidError is a request the guest refuses as asked, which the host answers 400 and not 500.
type invalidError string

func (e invalidError) Error() string { return string(e) }

// codeOf names a refusal the host maps to the API's own code; an empty one is the guest's fault, an internal error.
func codeOf(err error) string {
	var invalid invalidError
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return supervisor.FileNotFound
	case errors.As(err, &invalid), errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.EISDIR), errors.Is(err, syscall.ENOTDIR):
		return supervisor.FileInvalid
	}

	return ""
}

// openFile opens a get's source and refuses anything but a regular file: a fifo would block the open, a directory has no bytes.
func openFile(path string) (models.FileStat, *os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return models.FileStat{}, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return models.FileStat{}, nil, errors.Join(err, f.Close())
	}
	if info.IsDir() {
		return models.FileStat{}, nil, errors.Join(invalidError(path+" is a directory; a get takes one file"), f.Close())
	}
	if !info.Mode().IsRegular() {
		return models.FileStat{}, nil, errors.Join(invalidError(fmt.Sprintf("%s is a %s, not a regular file; a get takes one file", path, info.Mode().Type())), f.Close())
	}

	return statOf(info), f, nil
}

func closeSource(src *os.File) error {
	if src == nil {
		return nil
	}

	return src.Close()
}

func statOf(info fs.FileInfo) models.FileStat {
	stat := models.FileStat{Type: typeOf(info.Mode()), Size: info.Size(), Mode: modeOf(info.Mode()), MTime: info.ModTime().UTC()}
	if sys, ok := info.Sys().(*syscall.Stat_t); ok {
		stat.UID, stat.GID = sys.Uid, sys.Gid
	}

	return stat
}

func typeOf(mode fs.FileMode) models.FileType {
	switch {
	case mode.IsRegular():
		return models.FileRegular
	case mode.IsDir():
		return models.FileDir
	case mode&fs.ModeSymlink != 0:
		return models.FileSymlink
	}

	return models.FileOther
}

// modeOf is the octal mode a shell shows, since fs.FileMode keeps setuid, setgid and sticky outside the low twelve bits.
func modeOf(mode fs.FileMode) uint32 {
	bits := uint32(mode.Perm())
	if mode&fs.ModeSetuid != 0 {
		bits |= 0o4000
	}
	if mode&fs.ModeSetgid != 0 {
		bits |= 0o2000
	}
	if mode&fs.ModeSticky != 0 {
		bits |= 0o1000
	}

	return bits
}

// receiveFile takes the host's bytes into a temp name beside the target, so a copy that dies midway leaves the old file whole.
func receiveFile(r io.Reader, header supervisor.FileHeader) error {
	dir := filepath.Dir(header.Path)
	if header.Parents {
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: a parent reads as mkdir -p makes it, so a non-root entrypoint can still reach the file
			return err
		}
	}
	// The dir opens first, so a user who may not read it is refused before the old file changes.
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := placeFile(d, r, header); err != nil {
		return errors.Join(err, d.Close())
	}

	return d.Close()
}

// placeFile renames the filled temp over the target and syncs the dir, or a VM stopped past its grace can lose the rename.
func placeFile(d *os.File, r io.Reader, header supervisor.FileHeader) error {
	f, err := os.CreateTemp(d.Name(), ".shard-put-*")
	if err != nil {
		return err
	}
	if err := fillFile(f, r, header); err != nil {
		return errors.Join(err, removeTemp(f.Name()))
	}
	if err := os.Rename(f.Name(), header.Path); err != nil {
		return errors.Join(err, removeTemp(f.Name()))
	}
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync %s after the rename: %w", d.Name(), err)
	}

	return nil
}

// fillFile lands the bytes, the mode and the sync, and closes; the rename waits for the close so no reader sees a file still being written.
func fillFile(f *os.File, r io.Reader, header supervisor.FileHeader) error {
	if _, err := io.CopyN(f, r, header.Size); err != nil {
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
