package image

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/presmihaylov/shard/pkg/ext4"
)

// entry is one tar entry of a test layer; body is the content of a regular file.
type entry struct {
	hdr  tar.Header
	body string
}

func reg(name, body string) entry {
	return entry{hdr: tar.Header{Name: name, Mode: 0o644, Typeflag: tar.TypeReg}, body: body}
}

func dir(name string) entry {
	return entry{hdr: tar.Header{Name: name, Mode: 0o755, Typeflag: tar.TypeDir}}
}

func layerOf(t *testing.T, entries ...entry) v1.Layer {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := e.hdr
		hdr.Size = int64(len(e.body))
		hdr.ModTime = time.Unix(1700000000, 0)
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatalf("write the header for %s: %v", hdr.Name, err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatalf("write %s: %v", hdr.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close the tar: %v", err)
	}

	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
	})
	if err != nil {
		t.Fatalf("build the layer: %v", err)
	}

	return layer
}

// disk writes the layers through the merge and hands back the writer, still open, so a test can stat it.
func disk(t *testing.T, layers ...v1.Layer) (*ext4.Writer, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "rootfs.ext4")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create the image: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	w := ext4.NewWriter(f)
	if err := writeDisk(t.Context(), w, layers); err != nil {
		t.Fatalf("writeDisk: %v", err)
	}

	return w, path
}

func closeAndCheck(t *testing.T, w *ext4.Writer, path string) {
	t.Helper()
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := exec.LookPath("e2fsck"); err != nil {
		t.Logf("no e2fsck on PATH, the stats stand alone")

		return
	}
	out, err := exec.Command("e2fsck", "-fn", path).CombinedOutput()
	if err != nil {
		t.Fatalf("e2fsck: %v\n%s", err, out)
	}
}

func statSize(t *testing.T, w *ext4.Writer, name string) int64 {
	t.Helper()
	f, err := w.Stat(name)
	if err != nil {
		t.Fatalf("Stat %s: %v", name, err)
	}

	return f.Size
}

func missing(t *testing.T, w *ext4.Writer, name string) {
	t.Helper()
	if _, err := w.Stat(name); err == nil {
		t.Errorf("%s is on the disk", name)
	}
}

func TestDiskKeepsTheMetadataADirectoryUnpackLoses(t *testing.T) {
	sudo := entry{hdr: tar.Header{Name: "usr/bin/sudo", Mode: 0o4755, Typeflag: tar.TypeReg, Uid: 0, Gid: 0,
		PAXRecords: map[string]string{"SCHILY.xattr.security.capability": "\x01\x00\x00\x02"}}, body: "elf"}
	null := entry{hdr: tar.Header{Name: "dev/null", Mode: 0o666, Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3}}
	link := entry{hdr: tar.Header{Name: "bin/sh", Typeflag: tar.TypeSymlink, Linkname: "busybox"}}
	owned := entry{hdr: tar.Header{Name: "home/app/.profile", Mode: 0o600, Typeflag: tar.TypeReg, Uid: 1000, Gid: 1000}, body: "x"}

	w, path := disk(t, layerOf(t, dir("usr/"), dir("usr/bin/"), sudo, dir("dev/"), null, dir("bin/"), reg("bin/busybox", "bb"), link, owned))

	f, err := w.Stat("usr/bin/sudo")
	if err != nil {
		t.Fatalf("Stat sudo: %v", err)
	}
	if f.Mode != ext4.S_IFREG|0o4755 {
		t.Errorf("sudo mode %o", f.Mode)
	}
	if string(f.Xattrs["security.capability"]) != "\x01\x00\x00\x02" {
		t.Errorf("sudo xattrs %q", f.Xattrs)
	}

	f, err = w.Stat("dev/null")
	if err != nil {
		t.Fatalf("Stat dev/null: %v", err)
	}
	if f.Mode != ext4.S_IFCHR|0o666 || f.Devmajor != 1 || f.Devminor != 3 {
		t.Errorf("dev/null mode %o dev %d:%d", f.Mode, f.Devmajor, f.Devminor)
	}

	f, err = w.Stat("home/app/.profile")
	if err != nil {
		t.Fatalf("Stat .profile: %v", err)
	}
	if f.Uid != 1000 || f.Gid != 1000 {
		t.Errorf(".profile owner %d:%d", f.Uid, f.Gid)
	}

	// home/ came from no tar entry, so it gets the default root directory.
	f, err = w.Stat("home")
	if err != nil {
		t.Fatalf("Stat home: %v", err)
	}
	if f.Mode != ext4.S_IFDIR|0o755 || f.Uid != 0 {
		t.Errorf("home mode %o owner %d", f.Mode, f.Uid)
	}

	if got := statSize(t, w, "bin/sh"); got != int64(len("busybox")) {
		t.Errorf("bin/sh size %d", got)
	}

	closeAndCheck(t, w, path)
}

func TestDiskAppliesWhiteoutsAcrossLayers(t *testing.T) {
	base := layerOf(t, dir("etc/"), reg("etc/motd", "hello"), dir("opt/"), dir("opt/tool/"), reg("opt/tool/a", "a"), reg("opt/tool/b", "bb"), dir("var/"), reg("var/keep", "k"))
	top := layerOf(t,
		reg("etc/.wh.motd", ""),
		dir("opt/tool/"), reg("opt/tool/.wh..wh..opq", ""), reg("opt/tool/c", "ccc"),
		reg("var/keep", "kept-longer"))

	w, path := disk(t, base, top)

	missing(t, w, "etc/motd")
	missing(t, w, "opt/tool/a")
	missing(t, w, "opt/tool/b")
	if got := statSize(t, w, "opt/tool/c"); got != 3 {
		t.Errorf("opt/tool/c size %d", got)
	}
	if got := statSize(t, w, "var/keep"); got != int64(len("kept-longer")) {
		t.Errorf("var/keep size %d", got)
	}
	if _, err := w.Stat("etc"); err != nil {
		t.Errorf("etc went with its file: %v", err)
	}

	closeAndCheck(t, w, path)
}

func TestDiskADirectoryEntryKeepsTheFilesBelowIt(t *testing.T) {
	base := layerOf(t, dir("etc/"), reg("etc/motd", "hello"))
	top := layerOf(t, entry{hdr: tar.Header{Name: "etc/", Mode: 0o700, Typeflag: tar.TypeDir}}, reg("etc/issue", "i"))

	w, path := disk(t, base, top)

	if got := statSize(t, w, "etc/motd"); got != 5 {
		t.Errorf("etc/motd size %d", got)
	}
	f, err := w.Stat("etc")
	if err != nil {
		t.Fatalf("Stat etc: %v", err)
	}
	if f.Mode != ext4.S_IFDIR|0o700 {
		t.Errorf("etc mode %o", f.Mode)
	}

	closeAndCheck(t, w, path)
}

func TestDiskAWhiteoutTakesTheSubtree(t *testing.T) {
	base := layerOf(t, dir("opt/"), dir("opt/tool/"), reg("opt/tool/a", "a"))
	top := layerOf(t, reg("opt/.wh.tool", ""))

	w, path := disk(t, base, top)

	missing(t, w, "opt/tool")
	missing(t, w, "opt/tool/a")
	if _, err := w.Stat("opt"); err != nil {
		t.Errorf("opt went too: %v", err)
	}

	closeAndCheck(t, w, path)
}

// A hard link made in a lower layer keeps the content it linked, even when a higher layer replaces the target.
func TestDiskAHardLinkKeepsTheVersionItTook(t *testing.T) {
	hard := entry{hdr: tar.Header{Name: "bin/ls", Typeflag: tar.TypeLink, Linkname: "bin/busybox"}}
	base := layerOf(t, dir("bin/"), reg("bin/busybox", "old-busybox"), hard)
	top := layerOf(t, reg("bin/busybox", "new"))

	w, path := disk(t, base, top)

	if got := statSize(t, w, "bin/ls"); got != int64(len("old-busybox")) {
		t.Errorf("bin/ls size %d", got)
	}
	if got := statSize(t, w, "bin/busybox"); got != 3 {
		t.Errorf("bin/busybox size %d", got)
	}

	closeAndCheck(t, w, path)
}

// A whiteout of the target leaves the content to its link alone: no name of the target comes back.
func TestDiskAWhiteoutOfALinkTargetKeepsOnlyTheLink(t *testing.T) {
	alias := entry{hdr: tar.Header{Name: "bin/alias", Typeflag: tar.TypeLink, Linkname: "bin/tool"}}
	base := layerOf(t, dir("bin/"), reg("bin/tool", "tool-body"), alias)
	top := layerOf(t, reg("bin/.wh.tool", ""))

	w, path := disk(t, base, top)

	if got := statSize(t, w, "bin/alias"); got != int64(len("tool-body")) {
		t.Errorf("bin/alias size %d", got)
	}
	missing(t, w, "bin/tool")
	missing(t, w, ".shard-link0-0-1")

	closeAndCheck(t, w, path)
}

// A target whose every link is gone is gone too, whatever a lower layer linked to it.
func TestDiskAWhiteoutOfTheLinkAndTheTargetDropsBoth(t *testing.T) {
	alias := entry{hdr: tar.Header{Name: "bin/alias", Typeflag: tar.TypeLink, Linkname: "bin/tool"}}
	base := layerOf(t, dir("bin/"), reg("bin/tool", "tool-body"), alias)
	top := layerOf(t, reg("bin/.wh.tool", ""), reg("bin/.wh.alias", ""))

	w, path := disk(t, base, top)

	missing(t, w, "bin/tool")
	missing(t, w, "bin/alias")
	missing(t, w, ".shard-link0-0-1")

	closeAndCheck(t, w, path)
}

// A link to a link reaches the file behind both, even once the two earlier names are gone.
func TestDiskALinkToALinkOutlivesBothEarlierNames(t *testing.T) {
	first := entry{hdr: tar.Header{Name: "bin/first", Typeflag: tar.TypeLink, Linkname: "bin/tool"}}
	last := entry{hdr: tar.Header{Name: "bin/last", Typeflag: tar.TypeLink, Linkname: "bin/first"}}
	base := layerOf(t, dir("bin/"), reg("bin/tool", "tool-body"), first)
	mid := layerOf(t, last)
	top := layerOf(t, reg("bin/.wh.tool", ""), reg("bin/.wh.first", ""))

	w, path := disk(t, base, mid, top)

	if got := statSize(t, w, "bin/last"); got != int64(len("tool-body")) {
		t.Errorf("bin/last size %d", got)
	}
	missing(t, w, "bin/tool")
	missing(t, w, "bin/first")
	missing(t, w, ".shard-link0-0-1")

	closeAndCheck(t, w, path)
}

// An image file that spells a scratch name is left alone: the scratch prefix moves past it.
func TestDiskAScratchNameNeverTakesAnImagePath(t *testing.T) {
	alias := entry{hdr: tar.Header{Name: "bin/alias", Typeflag: tar.TypeLink, Linkname: "bin/tool"}}
	base := layerOf(t, dir("bin/"), reg("bin/tool", "tool-body"), alias)
	top := layerOf(t, reg("bin/.wh.tool", ""), reg(".shard-link0-0-1", "mine"))

	w, path := disk(t, base, top)

	if got := statSize(t, w, "bin/alias"); got != int64(len("tool-body")) {
		t.Errorf("bin/alias size %d", got)
	}
	if got := statSize(t, w, ".shard-link0-0-1"); got != 4 {
		t.Errorf(".shard-link0-0-1 size %d", got)
	}
	missing(t, w, "bin/tool")
	missing(t, w, ".shard-link1-0-1")

	closeAndCheck(t, w, path)
}

func TestDiskRefusesAHardLinkToNothing(t *testing.T) {
	hard := entry{hdr: tar.Header{Name: "bin/ls", Typeflag: tar.TypeLink, Linkname: "bin/busybox"}}

	path := filepath.Join(t.TempDir(), "rootfs.ext4")
	err := buildDisk(t.Context(), path, []v1.Layer{layerOf(t, dir("bin/"), hard)})
	if err == nil || !strings.Contains(err.Error(), "hard link to bin/busybox") {
		t.Fatalf("buildDisk: %v", err)
	}
}

func TestDiskStopsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	path := filepath.Join(t.TempDir(), "rootfs.ext4")
	err := buildDisk(ctx, path, []v1.Layer{layerOf(t, reg("a", "a"))})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("buildDisk: %v", err)
	}
}

// A cancel lands at the next read of the tar, not at the next header, so a large body cannot outlive it.
func TestDiskACancelEndsTheBodyMidCopy(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	r := readerOf(ctx, strings.NewReader("body"))

	buf := make([]byte, 2)
	if n, err := r.Read(buf); err != nil || n != 2 {
		t.Fatalf("read before the cancel: %d, %v", n, err)
	}
	cancel()
	if _, err := r.Read(buf); !errors.Is(err, context.Canceled) {
		t.Fatalf("read after the cancel: %v", err)
	}
}
