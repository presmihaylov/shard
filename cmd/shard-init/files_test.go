package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/supervisor"
)

// startFiles brings the guest up with an entrypoint running, as a host does, and answers a way to open one files exec.
func startFiles(t *testing.T) func() supervisor.FilesConn {
	t.Helper()
	_, dial := startTransport(t)
	ctx := testContext(t)
	c, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	// No app: the cleanup kills the supervisor, and an app would outlive it as an orphan (SHARD-481).
	if err := c.Run(ctx, supervisor.RunSpec{}); err != nil {
		t.Fatalf("run: %v", err)
	}

	// The path through the guest exec a VM takes: no /.shard/init exists here, so the guest must answer it with itself.
	run := func(ctx context.Context, spec models.ExecSpec) (models.ExitStatus, error) {
		return supervisor.Exec(ctx, dial, "sb", supervisor.ExecHeader{Argv: spec.Argv, WorkDir: spec.WorkDir}, spec)
	}

	return func() supervisor.FilesConn {
		conn, err := supervisor.OpenFiles(ctx, run, "")
		if err != nil {
			t.Fatalf("open a files exec: %v", err)
		}

		return conn
	}
}

// closeFiles ends a files exec and fails the test unless the guest exited cleanly.
func closeFiles(t *testing.T, conn io.Closer) {
	t.Helper()
	if err := conn.Close(); err != nil {
		t.Fatalf("close the files exec: %v", err)
	}
}

func statFile(t *testing.T, open func() supervisor.FilesConn, path string) (models.FileStat, error) {
	t.Helper()
	conn := open()
	defer closeFiles(t, conn)

	return supervisor.Stat(conn, path)
}

func getFile(t *testing.T, open func() supervisor.FilesConn, path string) (models.FileStat, []byte, error) {
	t.Helper()
	conn := open()
	defer closeFiles(t, conn)

	stat, body, err := supervisor.Get(conn, path)
	if err != nil {
		return models.FileStat{}, nil, err
	}
	got, err := io.ReadAll(body)

	return stat, got, err
}

func putFile(t *testing.T, open func() supervisor.FilesConn, header supervisor.FileHeader, src io.Reader) error {
	t.Helper()
	conn := open()
	defer closeFiles(t, conn)

	return supervisor.Put(conn, header, src)
}

func listEntries(t *testing.T, open func() supervisor.FilesConn, path string) ([]models.FileEntry, error) {
	t.Helper()
	conn := open()
	defer closeFiles(t, conn)

	entries, err := supervisor.List(conn, path)
	if err != nil {
		return nil, err
	}
	var got []models.FileEntry
	for {
		entry, err := entries.Next()
		if errors.Is(err, io.EOF) {
			return got, nil
		}
		if err != nil {
			return got, err
		}
		got = append(got, entry)
	}
}

func mkdirPath(t *testing.T, open func() supervisor.FilesConn, header supervisor.FileHeader) error {
	t.Helper()
	conn := open()
	defer closeFiles(t, conn)

	return supervisor.Mkdir(conn, header)
}

func deleteFile(t *testing.T, open func() supervisor.FilesConn, path string, recursive bool) error {
	t.Helper()
	conn := open()
	defer closeFiles(t, conn)

	return supervisor.Delete(conn, path, recursive)
}

// payload crosses the frame bound several times, so a copy that lands whole did not fit in one write.
func payload(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 3*supervisor.MaxPayload+17)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("fill the payload: %v", err)
	}

	return b
}

// refusedAs fails the test unless err is the guest's refusal with code and a message holding text.
func refusedAs(t *testing.T, what string, err error, code, text string) {
	t.Helper()
	var refusal *supervisor.FileError
	if !errors.As(err, &refusal) || refusal.Code != code || !strings.Contains(refusal.Message, text) {
		t.Fatalf("%s gave %v, want a %q refusal that says %q", what, err, code, text)
	}
}

// The host puts the op and the path in front of each refusal, so the guest's words must not name the path again (SHARD-407).
func TestFilesRefusalsNameThePathOnce(t *testing.T) {
	open := startFiles(t)
	dir := t.TempDir()
	full := filepath.Join(dir, "full")
	if err := os.MkdirAll(filepath.Join(full, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing")

	for _, c := range []struct {
		what, path string
		do         func() error
	}{
		{what: "a stat of a missing path", path: missing, do: func() error { _, err := statFile(t, open, missing); return err }},
		{what: "a get of a directory", path: full, do: func() error { _, _, err := getFile(t, open, full); return err }},
		{what: "a get of a fifo", path: fifo, do: func() error { _, _, err := getFile(t, open, fifo); return err }},
		{what: "an ls of a file", path: file, do: func() error { _, err := listEntries(t, open, file); return err }},
		{what: "a mkdir of a directory already there", path: full, do: func() error {
			return mkdirPath(t, open, supervisor.FileHeader{Path: full, Mode: 0o755})
		}},
		{what: "a mkdir with parents over a file", path: file, do: func() error {
			return mkdirPath(t, open, supervisor.FileHeader{Path: file, Mode: 0o755, Parents: true})
		}},
		{what: "a delete of a full directory", path: full, do: func() error { return deleteFile(t, open, full, false) }},
		{what: "a delete of a missing path", path: missing, do: func() error { return deleteFile(t, open, missing, true) }},
		{what: "a pack of a missing path", path: missing, do: func() error { _, _, err := getArchive(t, open, missing); return err }},
		{what: "an unpack into a file", path: file, do: func() error { return putArchive(t, open, file, tarOf(t)) }},
		{what: "an unpack into a missing path", path: missing, do: func() error { return putArchive(t, open, missing, tarOf(t)) }},
	} {
		var refusal *supervisor.FileError
		if err := c.do(); !errors.As(err, &refusal) || strings.Count(err.Error(), c.path) != 1 {
			t.Errorf("%s gave %v, want a refusal that names %s once", c.what, err, c.path)
		}
	}
}

func TestFilesStatReportsTheShape(t *testing.T) {
	open := startFiles(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o750|fs.ModeSetuid); err != nil {
		t.Fatal(err)
	}

	stat, err := statFile(t, open, path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if stat.Type != models.FileRegular || stat.Size != 5 || stat.Mode != 0o4750 {
		t.Fatalf("stat = %+v, want a 5 byte file with mode 4750", stat)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.UID != sys.Uid || stat.GID != sys.Gid || !stat.MTime.Equal(info.ModTime()) {
		t.Fatalf("stat = %+v, want the owner and mtime the host sees, %+v", stat, info)
	}

	folder, err := statFile(t, open, dir)
	if err != nil || folder.Type != models.FileDir {
		t.Fatalf("stat of the dir = %+v, %v, want a dir", folder, err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if got, err := statFile(t, open, link); err != nil || got.Type != models.FileSymlink {
		t.Fatalf("stat of a symlink = %+v, %v, want the link itself", got, err)
	}
}

func TestFilesPutLandsAWholeFile(t *testing.T) {
	open := startFiles(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "blob")
	want := payload(t)

	if err := putFile(t, open, supervisor.FileHeader{Path: path, Size: int64(len(want)), Mode: 0o600}, bytes.NewReader(want)); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the file holds %d bytes, want %d equal ones", len(got), len(want))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode())
	}
	assertNoTemp(t, dir)
}

func TestFilesPutMakesTheParentsOnlyWhenAsked(t *testing.T) {
	open := startFiles(t)
	path := filepath.Join(t.TempDir(), "a", "b", "note")

	err := putFile(t, open, supervisor.FileHeader{Path: path, Size: 2, Mode: 0o644}, strings.NewReader("hi"))
	refusedAs(t, "a put under a missing dir", err, supervisor.FileNotFound, "no such file")

	if err := putFile(t, open, supervisor.FileHeader{Path: path, Size: 2, Mode: 0o644, Parents: true}, strings.NewReader("hi")); err != nil {
		t.Fatalf("put with parents: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "hi" {
		t.Fatalf("the file reads %q (%v), want hi", got, err)
	}
}

func TestFilesGetStreamsTheFile(t *testing.T) {
	open := startFiles(t)
	path := filepath.Join(t.TempDir(), "blob")
	want := payload(t)
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}

	stat, got, err := getFile(t, open, path)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stat.Size != int64(len(want)) || !bytes.Equal(got, want) {
		t.Fatalf("get gave %d bytes with stat %+v, want %d equal ones", len(got), stat, len(want))
	}
}

func TestFilesRefuseWhatTheyCannotCopy(t *testing.T) {
	open := startFiles(t)
	dir := t.TempDir()

	_, _, err := getFile(t, open, dir)
	refusedAs(t, "a get of a directory", err, supervisor.FileInvalid, "is a directory")
	_, err = statFile(t, open, "relative/name")
	refusedAs(t, "a stat of a relative path", err, supervisor.FileInvalid, "must be absolute")
	_, err = statFile(t, open, filepath.Join(dir, "missing"))
	refusedAs(t, "a stat of a missing path", err, supervisor.FileNotFound, "no such file")

	// A fifo would block the guest's open forever, so the get refuses it before the reply.
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = getFile(t, open, fifo)
	refusedAs(t, "a get of a fifo", err, supervisor.FileInvalid, "is a named pipe, not a regular file")
	if got, err := statFile(t, open, fifo); err != nil || got.Type != models.FileOther {
		t.Fatalf("stat of a fifo gave %+v, %v, want other", got, err)
	}

	// The guest refuses the header, so its reason must beat the broken pipe the rest of the payload meets.
	want := payload(t)
	err = putFile(t, open, supervisor.FileHeader{Path: filepath.Join(dir, "nowhere", "blob"), Size: int64(len(want)), Mode: 0o600}, bytes.NewReader(want))
	refusedAs(t, "a put under a missing dir", err, supervisor.FileNotFound, "no such file")
}

func TestFilesPutThatDiesMidwayLeavesTheOldFile(t *testing.T) {
	open := startFiles(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "blob")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The source runs out before the promised size, so the host hangs up with the guest's copy short.
	want := payload(t)
	conn := open()
	err := supervisor.Put(conn, supervisor.FileHeader{Path: path, Size: int64(len(want)) + 1, Mode: 0o600}, bytes.NewReader(want))
	if err == nil {
		t.Fatal("a short put succeeded")
	}
	// Whether the guest's refusal of the short copy lands depends on the transport, so only the file is checked.
	_ = conn.Close()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "old" {
		t.Fatalf("the old file reads %q (%v), want it untouched", got, err)
	}
	assertNoTemp(t, dir)
}

func TestFilesListAnswersEachEntrySortedWithItsOwnStat(t *testing.T) {
	open := startFiles(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("hello"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "a"), filepath.Join(dir, "c")); err != nil {
		t.Fatal(err)
	}

	got, err := listEntries(t, open, dir)
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	// -1 skips a field: a symlink's own mode differs between Linux and macOS, a directory's size between filesystems.
	want := []struct {
		name string
		typ  models.FileType
		size int64
		mode int64
	}{{"a", models.FileDir, -1, 0o700}, {"b.txt", models.FileRegular, 5, 0o640}, {"c", models.FileSymlink, -1, -1}}
	if len(got) != len(want) {
		t.Fatalf("ls gave %+v, want %d entries", got, len(want))
	}
	for i, w := range want {
		if got[i].Name != w.name || got[i].Type != w.typ || (w.mode >= 0 && int64(got[i].Mode) != w.mode) || (w.size >= 0 && got[i].Size != w.size) {
			t.Fatalf("entry %d is %+v, want %s, a %s of mode %o and size %d", i, got[i], w.name, w.typ, w.mode, w.size)
		}
	}

	// A symlink to a directory lists what it points to, as ls does.
	through, err := listEntries(t, open, filepath.Join(dir, "c"))
	if err != nil || len(through) != 0 {
		t.Fatalf("ls through the link gave %+v, %v, want the empty directory a", through, err)
	}
}

// A listing past the frame bound proves the entries stream one line each, never as one reply.
func TestFilesListStreamsALargeDirectory(t *testing.T) {
	open := startFiles(t)
	dir := t.TempDir()
	const count = 5000
	name := strings.Repeat("n", 200)
	for i := range count {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s-%05d", name, i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := listEntries(t, open, dir)
	if err != nil || len(got) != count {
		t.Fatalf("ls gave %d entries, %v, want %d", len(got), err, count)
	}
	if got[count-1].Name != fmt.Sprintf("%s-%05d", name, count-1) {
		t.Fatalf("the last entry is %s, want the highest name", got[count-1].Name)
	}
}

func TestFilesListRefusesWhatIsNotADirectory(t *testing.T) {
	open := startFiles(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := listEntries(t, open, file)
	refusedAs(t, "an ls of a file", err, supervisor.FileInvalid, "not a directory")
	_, err = listEntries(t, open, fifo)
	refusedAs(t, "an ls of a fifo", err, supervisor.FileInvalid, "not a directory")
	_, err = listEntries(t, open, filepath.Join(dir, "missing"))
	refusedAs(t, "an ls of a missing path", err, supervisor.FileNotFound, "no such file")
}

func TestFilesMkdirSetsTheModePastTheUmask(t *testing.T) {
	open := startFiles(t)
	dir := t.TempDir()
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })

	path := filepath.Join(dir, "shared")
	if err := mkdirPath(t, open, supervisor.FileHeader{Path: path, Mode: 0o775}); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o775 {
		t.Fatalf("the directory is %v, %v, want a directory of mode 0775", info, err)
	}

	err = mkdirPath(t, open, supervisor.FileHeader{Path: path, Mode: 0o775})
	refusedAs(t, "a mkdir of a directory already there", err, supervisor.FileInvalid, "file exists")
}

func TestFilesMkdirWithParentsIsMkdirP(t *testing.T) {
	open := startFiles(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "c")

	err := mkdirPath(t, open, supervisor.FileHeader{Path: path, Mode: 0o700})
	refusedAs(t, "a mkdir under a missing dir", err, supervisor.FileNotFound, "no such file")

	if err := mkdirPath(t, open, supervisor.FileHeader{Path: path, Mode: 0o700, Parents: true}); err != nil {
		t.Fatalf("mkdir with parents: %v", err)
	}
	leaf, err := os.Stat(path)
	if err != nil || leaf.Mode().Perm() != 0o700 {
		t.Fatalf("the leaf is %v, %v, want mode 0700", leaf, err)
	}
	if parent, err := os.Stat(filepath.Dir(path)); err != nil || !parent.IsDir() {
		t.Fatalf("the parent is %v, %v, want a directory", parent, err)
	}

	// A directory already there is the success mkdir -p gives, and keeps its own mode.
	if err := mkdirPath(t, open, supervisor.FileHeader{Path: path, Mode: 0o755, Parents: true}); err != nil {
		t.Fatalf("mkdir with parents of a directory already there: %v", err)
	}
	if again, err := os.Stat(path); err != nil || again.Mode().Perm() != 0o700 {
		t.Fatalf("the leaf is %v, %v, want its mode 0700 untouched", again, err)
	}

	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err = mkdirPath(t, open, supervisor.FileHeader{Path: file, Mode: 0o755, Parents: true})
	refusedAs(t, "a mkdir with parents over a file", err, supervisor.FileInvalid, "not a directory")
}

func TestFilesDeleteRemovesTheLinkAndKeepsItsTarget(t *testing.T) {
	open := startFiles(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "keep"), []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if err := deleteFile(t, open, link, true); err != nil {
		t.Fatalf("delete the link: %v", err)
	}
	if _, err := os.Lstat(link); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the link is still there: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(target, "keep")); err != nil || string(got) != "kept" {
		t.Fatalf("the target reads %q, %v, want it untouched", got, err)
	}
}

func TestFilesDeleteTakesAFullDirectoryOnlyWhenRecursive(t *testing.T) {
	open := startFiles(t)
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	if err := os.MkdirAll(filepath.Join(tree, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "a", "b", "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	err := deleteFile(t, open, tree, false)
	refusedAs(t, "a delete of a full directory", err, supervisor.FileInvalid, "pass recursive=true")
	if _, err := os.Stat(filepath.Join(tree, "a", "b", "file")); err != nil {
		t.Fatalf("the refused delete still removed something: %v", err)
	}

	if err := deleteFile(t, open, tree, true); err != nil {
		t.Fatalf("a recursive delete: %v", err)
	}
	if _, err := os.Lstat(tree); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the tree is still there: %v", err)
	}

	empty := filepath.Join(dir, "empty")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := deleteFile(t, open, empty, false); err != nil {
		t.Fatalf("a delete of an empty directory: %v", err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := deleteFile(t, open, file, false); err != nil {
		t.Fatalf("a delete of a file: %v", err)
	}

	err = deleteFile(t, open, filepath.Join(dir, "missing"), true)
	refusedAs(t, "a delete of a missing path", err, supervisor.FileNotFound, "no such file")
}

func getArchive(t *testing.T, open func() supervisor.FilesConn, path string) (models.FileStat, []byte, error) {
	t.Helper()
	conn := open()
	defer closeFiles(t, conn)

	stat, body, err := supervisor.GetArchive(conn, path)
	if err != nil {
		return models.FileStat{}, nil, err
	}
	got, err := io.ReadAll(body)

	return stat, got, err
}

func putArchive(t *testing.T, open func() supervisor.FilesConn, path string, src io.Reader) error {
	t.Helper()
	conn := open()
	defer closeFiles(t, conn)

	return supervisor.PutArchive(conn, path, src)
}

// tarOf builds a tar by hand, a name and its body per entry, with a trailing slash for a directory and "->" for a symlink.
func tarOf(t *testing.T, entries ...[2]string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{Name: e[0], Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(e[1]))}
		if strings.HasSuffix(e[0], "/") {
			hdr.Typeflag, hdr.Mode, hdr.Size = tar.TypeDir, 0o755, 0
		}
		if target, ok := strings.CutPrefix(e[1], "->"); ok {
			hdr.Typeflag, hdr.Linkname, hdr.Size = tar.TypeSymlink, target, 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Size > 0 {
			if _, err := tw.Write([]byte(e[1])); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	return &buf
}

func TestFilesPackThenUnpackMovesADirectory(t *testing.T) {
	open := startFiles(t)
	src := filepath.Join(t.TempDir(), "app")
	if err := os.MkdirAll(filepath.Join(src, "bin"), 0o750); err != nil {
		t.Fatal(err)
	}
	big := payload(t)
	if err := os.WriteFile(filepath.Join(src, "bin", "run"), big, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("bin/run", filepath.Join(src, "current")); err != nil {
		t.Fatal(err)
	}

	stat, archive, err := getArchive(t, open, src)
	if err != nil || stat.Type != models.FileDir {
		t.Fatalf("get the archive: %+v, %v, want the stat of a dir", stat, err)
	}
	dst := t.TempDir()
	if err := putArchive(t, open, dst, bytes.NewReader(archive)); err != nil {
		t.Fatalf("put the archive: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dst, "app", "bin", "run"))
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("app/bin/run holds %d bytes, %v, want %d equal ones", len(got), err, len(big))
	}
	if target, err := os.Readlink(filepath.Join(dst, "app", "current")); err != nil || target != "bin/run" {
		t.Fatalf("app/current links to %q, %v, want bin/run", target, err)
	}
	if info, err := os.Stat(filepath.Join(dst, "app", "bin")); err != nil || info.Mode().Perm() != 0o750 {
		t.Fatalf("app/bin is %v, %v, want mode 0750", info, err)
	}
}

// A guest that refuses an entry stops reading, so a host still sending a large archive gets the reason and not a broken pipe.
func TestFilesUnpackRefusesAnEntryThatEscapes(t *testing.T) {
	open := startFiles(t)
	parent := t.TempDir()
	dst := filepath.Join(parent, "dst")
	if err := os.Mkdir(dst, 0o755); err != nil {
		t.Fatal(err)
	}

	cases := map[string]*bytes.Buffer{
		"a ../ entry":               tarOf(t, [2]string{"../x", "x"}, [2]string{"big", string(payload(t))}),
		"an entry under a link out": tarOf(t, [2]string{"l", "->" + parent}, [2]string{"l/x", "x"}),
	}
	for name, archive := range cases {
		refusedAs(t, name, putArchive(t, open, dst, archive), supervisor.FileInvalid, "refuse the entry")
	}
	if _, err := os.Stat(filepath.Join(parent, "x")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("an escaping entry landed beside dst: %v", err)
	}
}

func TestFilesArchiveRefusesWhatItCannotTake(t *testing.T) {
	open := startFiles(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	refusedAs(t, "an unpack into a file", putArchive(t, open, file, tarOf(t)), supervisor.FileInvalid, "not a directory")
	refusedAs(t, "an unpack into nothing", putArchive(t, open, filepath.Join(dir, "missing"), tarOf(t)), supervisor.FileNotFound, "no such file")
	refusedAs(t, "an unpack of what is not a tar", putArchive(t, open, dir, strings.NewReader(strings.Repeat("not a tar ", 100))), supervisor.FileInvalid, "invalid tar header")
	_, _, err := getArchive(t, open, "/")
	refusedAs(t, "a pack of /", err, supervisor.FileInvalid, "has no name")
	_, _, err = getArchive(t, open, filepath.Join(dir, "missing"))
	refusedAs(t, "a pack of nothing", err, supervisor.FileNotFound, "no such file")
}

func TestLookPathAnswersTheInitPathWithItself(t *testing.T) {
	got, err := lookPath(entrypoint{argv: []string{supervisor.InitPath, supervisor.FilesMode}})
	if err != nil || got != selfBinary {
		t.Fatalf("lookPath = %q, %v, want this binary %q", got, err, selfBinary)
	}
}

func hasTemp(t *testing.T, dir string) bool {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".shard-put-*"))
	if err != nil {
		t.Fatal(err)
	}

	return len(matches) > 0
}

func assertNoTemp(t *testing.T, dir string) {
	t.Helper()
	if hasTemp(t, dir) {
		t.Fatalf("a .shard-put temp name is left under %s", dir)
	}
}

func TestFailTellsTheAttachedHostBeforeTheExit(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	tr := &transport{control: guest, attached: make(chan struct{}, 1)}
	tr.g = newGuest(tr, restartPolicy{})

	cause := errors.New("power off: no such device")
	failed := make(chan error, 1)
	go func() { failed <- tr.fail(errors.Join(errSupervisor, cause)) }()

	_ = host.SetReadDeadline(time.Now().Add(5 * time.Second))
	var m supervisor.Message
	if err := supervisor.ReadMessage(bufio.NewReader(host), &m); err != nil {
		t.Fatalf("read the death: %v", err)
	}
	if m.Kind != supervisor.KindSupervisorFailed || m.Exit == nil || m.Exit.Code != models.SupervisorFailedExitCode || !strings.Contains(m.Error, cause.Error()) {
		t.Fatalf("the host read %+v, want supervisor-failed with code 125 and the cause", m)
	}
	err := <-failed
	if exitCodeFor(err) != models.SupervisorFailedExitCode {
		t.Fatalf("fail returned %v, which maps to %d, want 125", err, exitCodeFor(err))
	}
}

// hasControl says whether a host is attached, which a test waits on after a hang-up.
func (t *transport) hasControl() bool {
	t.controlMu.Lock()
	defer t.controlMu.Unlock()

	return t.control != nil
}

// deadTransport is a guest whose loop has ended: the listeners are up, the control port accepts, and nobody runs supervise.
func deadTransport(t *testing.T) (*transport, supervisor.Dialer) {
	t.Helper()
	dir := shortDir(t)
	l, err := net.Listen("unix", filepath.Join(dir, fmt.Sprintf("%d.sock", supervisor.ControlPort)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	tr := &transport{attached: make(chan struct{}, 1)}
	tr.g = newGuest(tr, restartPolicy{})
	go tr.acceptControl(l)

	return tr, func(ctx context.Context, port uint32) (net.Conn, error) {
		var d net.Dialer

		return d.DialContext(ctx, "unix", filepath.Join(dir, fmt.Sprintf("%d.sock", port)))
	}
}

// expectDeath reads the state replay and then the death a host attaching after the supervisor failed must hear.
func expectDeath(t *testing.T, c *supervisor.Control) {
	t.Helper()
	for _, want := range []string{supervisor.KindState, supervisor.KindSupervisorFailed} {
		m, err := c.Next()
		if err != nil || m.Kind != want {
			t.Fatalf("the host read %+v (%v), want %s", m, err, want)
		}
	}
}

func TestFailWaitsForTheFirstHost(t *testing.T) {
	tr, dial := deadTransport(t)
	failed := make(chan error, 1)
	go func() { failed <- tr.fail(errSupervisor) }()

	// The host attaches a moment later, the way one does while the guest still boots, through the real attach.
	time.Sleep(50 * time.Millisecond)
	c, err := supervisor.Connect(testContext(t), dial)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	expectDeath(t, c)
	if err := <-failed; !errors.Is(err, errSupervisor) {
		t.Fatalf("fail returned %v, want the supervisor error", err)
	}
}

func TestFailWaitsPastAHostThatLeft(t *testing.T) {
	tr, dial := deadTransport(t)
	ctx := testContext(t)
	// The guest loop is still alive while the first host comes and goes.
	alive := make(chan struct{})
	go func() {
		for {
			select {
			case command := <-tr.g.commands:
				command()
			case <-alive:
				return
			}
		}
	}()
	first, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := first.Next(); err != nil || m.Kind != supervisor.KindState {
		t.Fatalf("the first host read %+v, %v", m, err)
	}
	// The first host hangs up before the death; the guest must forget it and wait for the next.
	first.Close()
	for tr.hasControl() {
		time.Sleep(10 * time.Millisecond)
	}
	close(alive)

	failed := make(chan error, 1)
	go func() { failed <- tr.fail(errSupervisor) }()
	time.Sleep(50 * time.Millisecond)
	second, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	expectDeath(t, second)
	if err := <-failed; !errors.Is(err, errSupervisor) {
		t.Fatalf("fail returned %v, want the supervisor error", err)
	}
}

func TestFailGivesUpWhenNoHostComes(t *testing.T) {
	old := failureGrace
	failureGrace = 100 * time.Millisecond
	t.Cleanup(func() { failureGrace = old })
	tr := &transport{attached: make(chan struct{}, 1)}
	tr.g = newGuest(tr, restartPolicy{})

	err := tr.fail(errSupervisor)
	if !errors.Is(err, errSupervisor) || !strings.Contains(err.Error(), "no host attached") {
		t.Fatalf("fail returned %v, want the supervisor error and no host", err)
	}
	if exitCodeFor(err) != models.SupervisorFailedExitCode {
		t.Fatalf("the exit code is %d, want 125", exitCodeFor(err))
	}
}

// deadConn is a host whose socket went away: every write fails.
type deadConn struct{ net.Conn }

func (deadConn) Write([]byte) (int, error) { return 0, syscall.EPIPE }

// A write that fails forgets that host and no other, so a replacement that attached meanwhile keeps its seat.
func TestATellThatFailsForgetsOnlyThatHost(t *testing.T) {
	tr := &transport{attached: make(chan struct{}, 1)}
	tr.g = newGuest(tr, restartPolicy{})
	gone, other := net.Pipe()
	defer other.Close()
	tr.control = deadConn{gone}

	heard, err := tr.tell(supervisor.Message{Kind: supervisor.KindState})
	if !heard || err == nil {
		t.Fatalf("tell = %v, %v; want heard and the write error", heard, err)
	}
	if tr.hasControl() {
		t.Fatal("the dead host is still attached")
	}

	host, peer := net.Pipe()
	defer peer.Close()
	tr.control = host
	tr.detach(deadConn{gone})
	if !tr.hasControl() {
		t.Fatal("a detach of the dead host cleared its replacement")
	}
}

// The first host's socket died under the report, and a second attaches while fail waits: the second hears the death.
func TestFailReachesAHostThatReplacedADeadOne(t *testing.T) {
	tr, dial := deadTransport(t)
	gone, other := net.Pipe()
	defer other.Close()
	tr.control = deadConn{gone}

	failed := make(chan error, 1)
	go func() { failed <- tr.fail(errSupervisor) }()
	time.Sleep(50 * time.Millisecond)
	second, err := supervisor.Connect(testContext(t), dial)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	expectDeath(t, second)
	if err := <-failed; !errors.Is(err, errSupervisor) {
		t.Fatalf("fail returned %v, want the supervisor error", err)
	}
}
