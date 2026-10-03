//go:build integration

package cli

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/services/sandbox"
)

// cpTestSize is large enough that no layer could pass it in one buffer, and small enough for every provider's disk.
const cpTestSize = 64 << 20

func sha256Of(t *testing.T, p string) string {
	t.Helper()

	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("open %s: %v", p, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash %s: %v", p, err)
	}

	return hex.EncodeToString(h.Sum(nil))
}

// SHARD-286: a file goes in and comes back out byte exact, with its mode.
func TestCpRoundTripsAFileByteExact(t *testing.T) {
	app, id := runningSandbox(t)

	src := filepath.Join(t.TempDir(), "blob")
	f, err := os.OpenFile(src, os.O_CREATE|os.O_WRONLY, 0o750)
	if err != nil {
		t.Fatalf("create %s: %v", src, err)
	}
	if _, err := io.CopyN(f, rand.NewChaCha8([32]byte{2, 8, 6}), cpTestSize); err != nil {
		t.Fatalf("write %s: %v", src, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", src, err)
	}
	want := sha256Of(t, src)

	if err := app.Run(t.Context(), []string{"cp", src, id + ":/tmp/"}); err != nil {
		t.Fatalf("cp in: %v", err)
	}
	out, err := runExec(t, app, "exec", id, "--", "/bin/sh", "-c", "sha256sum /tmp/blob; stat -c %a /tmp/blob")
	if err != nil || !strings.Contains(out, want) || !strings.HasSuffix(strings.TrimSpace(out), "750") {
		t.Fatalf("the guest reads %q, %v; want sha %s and mode 750", out, err, want)
	}

	back := filepath.Join(t.TempDir(), "back")
	if err := app.Run(t.Context(), []string{"cp", id + ":/tmp/blob", back}); err != nil {
		t.Fatalf("cp out: %v", err)
	}
	if got := sha256Of(t, back); got != want {
		t.Fatalf("the copy out hashes to %s, want %s", got, want)
	}
}

// failingReader hands over n bytes and then fails, as a client that dies midway does.
type failingReader struct {
	r io.Reader
}

func (f *failingReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if errors.Is(err, io.EOF) {
		return n, errors.New("the client went away")
	}

	return n, err
}

// SHARD-286: a put that dies midway leaves the old file whole and no temp name beside it.
func TestCpPutThatDiesMidwayLeavesTheOldFileWhole(t *testing.T) {
	app, id := runningSandbox(t)

	old := filepath.Join(t.TempDir(), "keep")
	if err := os.WriteFile(old, []byte("the old file\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", old, err)
	}
	if err := app.Run(t.Context(), []string{"cp", old, id + ":/tmp/keep"}); err != nil {
		t.Fatalf("cp in: %v", err)
	}

	cut := &failingReader{r: io.LimitReader(rand.NewChaCha8([32]byte{}), cpTestSize/2)}
	err := app.client().PutFile(t.Context(), id, sandbox.FileWrite{Path: "/tmp/keep", Mode: 0o644, Size: cpTestSize}, cut)
	if err == nil {
		t.Fatal("a put cut at half its size succeeded")
	}

	out, err := runExec(t, app, "exec", id, "--", "/bin/sh", "-c", "cat /tmp/keep; ls -a /tmp")
	if err != nil || !strings.HasPrefix(out, "the old file\n") || strings.Contains(out, ".shard-put-") {
		t.Fatalf("the guest holds %q, %v; want the old file whole and no temp name", out, err)
	}
}

// SHARD-286: --user runs the put as that user, so the guest kernel owns the file and checks the permission.
func TestCpAsAUserOwnsTheFileAndMeetsItsPermissions(t *testing.T) {
	app, id := runningSandbox(t)

	src := filepath.Join(t.TempDir(), "owned")
	if err := os.WriteFile(src, []byte("mine\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", src, err)
	}

	if err := app.Run(t.Context(), []string{"cp", "--user", "nobody", src, id + ":/tmp/owned"}); err != nil {
		t.Fatalf("cp as nobody: %v", err)
	}
	if out, err := runExec(t, app, "exec", id, "--", "/bin/stat", "-c", "%U", "/tmp/owned"); err != nil || strings.TrimSpace(out) != "nobody" {
		t.Fatalf("the file is owned by %q, %v; want nobody", out, err)
	}

	err := app.Run(t.Context(), []string{"cp", "--user", "nobody", src, id + ":/etc/owned"})
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("a put into /etc as nobody gave %v, want permission denied", err)
	}
}

// SHARD-288: a directory goes in and comes back out with its files, its modes and its symlinks.
func TestCpRoundTripsADirectory(t *testing.T) {
	app, id := runningSandbox(t)

	src := tree(t, t.TempDir(), "app")
	if err := app.Run(t.Context(), []string{"cp", src, id + ":/tmp/"}); err != nil {
		t.Fatalf("cp in: %v", err)
	}

	back := t.TempDir()
	if err := app.Run(t.Context(), []string{"cp", id + ":/tmp/app", back}); err != nil {
		t.Fatalf("cp out: %v", err)
	}
	checkTree(t, filepath.Join(back, "app"))
}

// SHARD-288: --user unpacks a directory as that user, so the guest kernel owns every entry and checks every permission.
func TestCpOfADirectoryAsAUserMeetsItsPermissions(t *testing.T) {
	app, id := runningSandbox(t)

	src := tree(t, t.TempDir(), "app")
	if err := app.Run(t.Context(), []string{"cp", "--user", "nobody", src, id + ":/tmp/owned"}); err != nil {
		t.Fatalf("cp as nobody: %v", err)
	}
	if out, err := runExec(t, app, "exec", id, "--", "/bin/stat", "-c", "%U", "/tmp/owned/sub/run.sh"); err != nil || strings.TrimSpace(out) != "nobody" {
		t.Fatalf("the file is owned by %q, %v; want nobody", out, err)
	}

	err := app.Run(t.Context(), []string{"cp", "--user", "nobody", src, id + ":/etc/owned"})
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("a copy into /etc as nobody gave %v, want permission denied", err)
	}
}

// SHARD-288: the guest refuses an archive entry that leaves the destination, and lands nothing past it.
func TestPutArchiveRefusesAnEntryThatLeaves(t *testing.T) {
	app, id := runningSandbox(t)

	if out, err := runExec(t, app, "exec", id, "--", "/bin/mkdir", "/tmp/in"); err != nil {
		t.Fatalf("make /tmp/in: %q, %v", out, err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}); err != nil {
		t.Fatalf("write the header: %v", err)
	}
	if _, err := tw.Write([]byte("x")); err != nil {
		t.Fatalf("write the body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close the tar: %v", err)
	}

	err := app.client().PutArchive(t.Context(), id, "/tmp/in", "", &buf)
	if err == nil || !strings.Contains(err.Error(), "../escape") {
		t.Fatalf("an archive that climbs out gave %v, want its refusal", err)
	}
	if out, err := runExec(t, app, "exec", id, "--", "/bin/ls", "-a", "/tmp"); err != nil || strings.Contains(out, "escape") {
		t.Fatalf("the guest /tmp holds %q, %v; want no escape", out, err)
	}
}
