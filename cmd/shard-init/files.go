package main

import (
	"archive/tar"
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/tarball"
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

	out, err := serveFile(r, header)
	if err != nil {
		return supervisor.WriteMessage(w, supervisor.FileReply{Error: refusalOf(header.Path, err), Code: codeOf(err)})
	}
	if out.entries != nil {
		return sendEntries(w, out)
	}
	if out.archive != "" {
		return sendArchive(w, out)
	}
	if err := supervisor.WriteMessage(w, supervisor.FileReply{Stat: &out.stat}); err != nil {
		return errors.Join(err, closeSource(out.file))
	}
	if out.file == nil {
		return nil
	}
	// Plain reads to EOF: a /proc or sysfs file's size is not its length, and a zero-copy path would trust it.
	if _, err := io.Copy(struct{ io.Writer }{w}, struct{ io.Reader }{out.file}); err != nil {
		return errors.Join(fmt.Errorf("send %s: %w", header.Path, err), out.file.Close())
	}

	return out.file.Close()
}

// served is one operation's answer: the stat the reply carries, and a get's open file, an ls's entries or a pack's path to follow it.
type served struct {
	stat    models.FileStat
	file    *os.File
	entries []models.FileEntry
	archive string
}

// sendArchive writes the reply and then the tar, buffered, since a tar is many small headers; a failure midway exits 1, which the host reads as the cut.
func sendArchive(w io.Writer, out served) error {
	buffered := bufio.NewWriterSize(w, 64<<10)
	if err := supervisor.WriteMessage(buffered, supervisor.FileReply{Stat: &out.stat}); err != nil {
		return err
	}
	if err := tarball.Pack(buffered, out.archive, filepath.Base(out.archive)); err != nil {
		return fmt.Errorf("pack %s: %w", out.archive, err)
	}
	if err := buffered.Flush(); err != nil {
		return fmt.Errorf("send the archive of %s: %w", out.archive, err)
	}

	return nil
}

// sendEntries writes the reply and then one line per entry, buffered, since a large directory is many small lines.
func sendEntries(w io.Writer, out served) error {
	buffered := bufio.NewWriter(w)
	if err := supervisor.WriteMessage(buffered, supervisor.FileReply{Stat: &out.stat, Count: len(out.entries)}); err != nil {
		return err
	}
	for _, entry := range out.entries {
		if err := supervisor.WriteMessage(buffered, entry); err != nil {
			return fmt.Errorf("send the entry %s: %w", entry.Name, err)
		}
	}
	if err := buffered.Flush(); err != nil {
		return fmt.Errorf("send the entries: %w", err)
	}

	return nil
}

// serveFile does the operation and, for a get, hands back the open file, so what the reply describes is what the bytes come from.
func serveFile(r io.Reader, header supervisor.FileHeader) (served, error) {
	if !filepath.IsAbs(header.Path) {
		return served{}, invalidError(fmt.Sprintf("a guest path must be absolute, got %q", header.Path))
	}

	switch header.Op {
	case supervisor.OpStat:
		return lstat(header.Path)
	case supervisor.OpGet:
		return openFile(header.Path)
	case supervisor.OpPut:
		if err := receiveFile(r, header); err != nil {
			return served{}, err
		}

		return lstat(header.Path)
	case supervisor.OpList:
		return listDir(header.Path)
	case supervisor.OpMkdir:
		return makeDir(header)
	case supervisor.OpDelete:
		return deletePath(header)
	case supervisor.OpPack:
		return packPath(header.Path)
	case supervisor.OpUnpack:
		if err := unpackInto(r, header.Path); err != nil {
			return served{}, err
		}

		return lstat(header.Path)
	default:
		return served{}, invalidError(fmt.Sprintf("unknown files op %q", header.Op))
	}
}

func lstat(path string) (served, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return served{}, err
	}

	return served{stat: statOf(info)}, nil
}

// listDir reads the whole directory before the reply, so the reply's count is what follows; a non-nil list marks an ls.
func listDir(path string) (served, error) {
	// O_DIRECTORY refuses anything but a directory at the open, so a fifo never blocks it.
	dir, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_DIRECTORY, 0)
	if err != nil {
		return served{}, err
	}
	info, err := dir.Stat()
	if err != nil {
		return served{}, errors.Join(err, dir.Close())
	}
	dirents, err := dir.ReadDir(-1)
	if err := errors.Join(err, dir.Close()); err != nil {
		return served{}, err
	}

	slices.SortFunc(dirents, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	entries := make([]models.FileEntry, 0, len(dirents))
	for _, dirent := range dirents {
		entry, err := dirent.Info()
		// A name removed between the read and its lstat is gone, which is what a later ls would say too.
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return served{}, err
		}
		entries = append(entries, models.FileEntry{Name: dirent.Name(), FileStat: statOf(entry)})
	}

	return served{stat: statOf(info), entries: entries}, nil
}

// makeDir sets the leaf's mode past the umask; with parents it makes what leads to it and takes a directory already there, as mkdir -p does.
func makeDir(header supervisor.FileHeader) (served, error) {
	if header.Parents {
		if err := os.MkdirAll(filepath.Dir(header.Path), 0o755); err != nil { //nolint:gosec // G301: a parent reads as mkdir -p makes it, so a non-root entrypoint can still reach the leaf
			return served{}, err
		}
	}

	mode := fs.FileMode(header.Mode).Perm()
	err := os.Mkdir(header.Path, mode)
	if errors.Is(err, fs.ErrExist) && header.Parents {
		return existingDir(header.Path)
	}
	if err != nil {
		return served{}, err
	}
	if err := os.Chmod(header.Path, mode); err != nil {
		return served{}, err
	}

	return lstat(header.Path)
}

// existingDir takes what a mkdir -p found in place, which follows a symlink to a directory and refuses anything else.
func existingDir(path string) (served, error) {
	info, err := os.Stat(path)
	if err != nil {
		return served{}, err
	}
	if !info.IsDir() {
		return served{}, invalidError("exists and is not a directory")
	}

	return served{stat: statOf(info)}, nil
}

// deletePath removes the path itself, so a final symlink goes and its target stays; the reply is the stat of what it removed.
func deletePath(header supervisor.FileHeader) (served, error) {
	info, err := os.Lstat(header.Path)
	if err != nil {
		return served{}, err
	}

	remove := os.Remove
	if header.Recursive {
		remove = os.RemoveAll
	}
	err = remove(header.Path)
	// Linux answers rmdir of a full directory with ENOTEMPTY, and POSIX allows EEXIST.
	if info.IsDir() && (errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)) {
		return served{}, invalidError("is a directory that is not empty; pass recursive=true to delete it and everything in it")
	}
	if err != nil {
		return served{}, err
	}

	return served{stat: statOf(info)}, nil
}

// packPath answers the stat of what a pack takes, which is never a final symlink's target; / has no base name to put at the top of a tar.
func packPath(path string) (served, error) {
	if filepath.Clean(path) == "/" {
		return served{}, invalidError("an archive of / has no name for its top entry; name a directory under it")
	}

	out, err := lstat(path)
	if err != nil {
		return served{}, err
	}
	out.archive = path

	return out, nil
}

// unpackInto lands the host's tar under dir, then drains stdin, so the host's end of the archive is what ends the op; the sync is for a clone, which reads the disk.
func unpackInto(r io.Reader, dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return invalidError("is not a directory; an archive unpacks into one")
	}

	if err := tarball.Unpack(r, dir, tarball.Options{KeepSetid: true}); err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		return fmt.Errorf("read past the end of the archive: %w", err)
	}
	syncDisks()

	return nil
}

// refusalOf is the guest's words for err; the host names the op and the path in front of them, so an error on the path itself drops its own.
func refusalOf(path string, err error) string {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) && pathErr.Path == path && err.Error() == pathErr.Error() {
		return pathErr.Err.Error()
	}

	return err.Error()
}

// invalidError is a request the guest refuses as asked, which the host answers 400 and not 500.
type invalidError string

func (e invalidError) Error() string { return string(e) }

// codeOf names a refusal the host maps to the API's own code; an empty one is the guest's fault, an internal error.
func codeOf(err error) string {
	var invalid invalidError
	var refused *tarball.RefusedError
	switch {
	case errors.As(err, &refused), errors.Is(err, tar.ErrHeader):
		return supervisor.FileInvalid
	case errors.Is(err, fs.ErrNotExist):
		return supervisor.FileNotFound
	case errors.As(err, &invalid), errors.Is(err, fs.ErrPermission), errors.Is(err, fs.ErrExist), errors.Is(err, syscall.EISDIR), errors.Is(err, syscall.ENOTDIR), errors.Is(err, syscall.ENOTEMPTY):
		return supervisor.FileInvalid
	}

	return ""
}

// openFile opens a get's source and refuses anything but a regular file: a fifo would block the open, a directory has no bytes.
func openFile(path string) (served, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return served{}, err
	}
	info, err := f.Stat()
	if err != nil {
		return served{}, errors.Join(err, f.Close())
	}
	if info.IsDir() {
		return served{}, errors.Join(invalidError("is a directory; a get takes one file"), f.Close())
	}
	if !info.Mode().IsRegular() {
		return served{}, errors.Join(invalidError(fmt.Sprintf("is a %s, not a regular file; a get takes one file", info.Mode().Type())), f.Close())
	}

	return served{stat: statOf(info), file: f}, nil
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
