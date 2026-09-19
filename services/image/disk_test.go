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

// stream plans the layers and reads back the merged tar the image writer would get, by name.
func stream(t *testing.T, layers ...v1.Layer) map[string]entry {
	t.Helper()

	m, err := planDisk(t.Context(), layers)
	if err != nil {
		t.Fatalf("planDisk: %v", err)
	}
	var buf bytes.Buffer
	if err := m.writeTar(t.Context(), &buf, layers); err != nil {
		t.Fatalf("writeTar: %v", err)
	}

	got := map[string]entry{}
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return got
		}
		if err != nil {
			t.Fatalf("read the merged tar: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s: %v", hdr.Name, err)
		}
		if _, dup := got[hdr.Name]; dup {
			t.Fatalf("%s is in the stream twice", hdr.Name)
		}
		got[hdr.Name] = entry{hdr: *hdr, body: string(body)}
	}
}

func file(t *testing.T, got map[string]entry, name string) entry {
	t.Helper()
	e, ok := got[name]
	if !ok {
		t.Fatalf("%s is not in the stream", name)
	}

	return e
}

func bodyOf(t *testing.T, got map[string]entry, name string) string {
	t.Helper()
	e := file(t, got, name)
	if e.hdr.Typeflag != tar.TypeReg {
		t.Fatalf("%s is a %q, not a file", name, e.hdr.Typeflag)
	}

	return e.body
}

func missing(t *testing.T, got map[string]entry, name string) {
	t.Helper()
	if _, ok := got[name]; ok {
		t.Errorf("%s is in the stream", name)
	}
}

func TestDiskKeepsTheMetadataADirectoryUnpackLoses(t *testing.T) {
	sudo := entry{hdr: tar.Header{Name: "usr/bin/sudo", Mode: 0o4755, Typeflag: tar.TypeReg, Uid: 0, Gid: 0,
		PAXRecords: map[string]string{"SCHILY.xattr.security.capability": "\x01\x00\x00\x02"}}, body: "elf"}
	null := entry{hdr: tar.Header{Name: "dev/null", Mode: 0o666, Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3}}
	link := entry{hdr: tar.Header{Name: "bin/sh", Typeflag: tar.TypeSymlink, Linkname: "busybox"}}
	owned := entry{hdr: tar.Header{Name: "home/app/.profile", Mode: 0o600, Typeflag: tar.TypeReg, Uid: 1000, Gid: 1000}, body: "x"}

	got := stream(t, layerOf(t, dir("usr/"), dir("usr/bin/"), sudo, dir("dev/"), null, dir("bin/"), reg("bin/busybox", "bb"), link, owned))

	if f := file(t, got, "usr/bin/sudo"); f.hdr.Mode != 0o4755 || f.hdr.PAXRecords["SCHILY.xattr.security.capability"] != "\x01\x00\x00\x02" {
		t.Errorf("sudo mode %o xattrs %q", f.hdr.Mode, f.hdr.PAXRecords)
	}
	if f := file(t, got, "dev/null"); f.hdr.Typeflag != tar.TypeChar || f.hdr.Mode != 0o666 || f.hdr.Devmajor != 1 || f.hdr.Devminor != 3 {
		t.Errorf("dev/null type %q mode %o dev %d:%d", f.hdr.Typeflag, f.hdr.Mode, f.hdr.Devmajor, f.hdr.Devminor)
	}
	if f := file(t, got, "home/app/.profile"); f.hdr.Uid != 1000 || f.hdr.Gid != 1000 {
		t.Errorf(".profile owner %d:%d", f.hdr.Uid, f.hdr.Gid)
	}
	// home/ came from no tar entry: the image writer makes it on the way to .profile.
	missing(t, got, "home")
	if f := file(t, got, "bin/sh"); f.hdr.Typeflag != tar.TypeSymlink || f.hdr.Linkname != "busybox" {
		t.Errorf("bin/sh type %q -> %s", f.hdr.Typeflag, f.hdr.Linkname)
	}
}

func TestDiskAppliesWhiteoutsAcrossLayers(t *testing.T) {
	base := layerOf(t, dir("etc/"), reg("etc/motd", "hello"), dir("opt/"), dir("opt/tool/"), reg("opt/tool/a", "a"), reg("opt/tool/b", "bb"), dir("var/"), reg("var/keep", "k"))
	top := layerOf(t,
		reg("etc/.wh.motd", ""),
		dir("opt/tool/"), reg("opt/tool/.wh..wh..opq", ""), reg("opt/tool/c", "ccc"),
		reg("var/keep", "kept-longer"))

	got := stream(t, base, top)

	missing(t, got, "etc/motd")
	missing(t, got, "opt/tool/a")
	missing(t, got, "opt/tool/b")
	missing(t, got, "etc/.wh.motd")
	missing(t, got, "opt/tool/.wh..wh..opq")
	if body := bodyOf(t, got, "opt/tool/c"); body != "ccc" {
		t.Errorf("opt/tool/c is %q", body)
	}
	if body := bodyOf(t, got, "var/keep"); body != "kept-longer" {
		t.Errorf("var/keep is %q", body)
	}
	file(t, got, "etc")
}

func TestDiskADirectoryEntryKeepsTheFilesBelowIt(t *testing.T) {
	base := layerOf(t, dir("etc/"), reg("etc/motd", "hello"))
	top := layerOf(t, entry{hdr: tar.Header{Name: "etc/", Mode: 0o700, Typeflag: tar.TypeDir}}, reg("etc/issue", "i"))

	got := stream(t, base, top)

	if body := bodyOf(t, got, "etc/motd"); body != "hello" {
		t.Errorf("etc/motd is %q", body)
	}
	if f := file(t, got, "etc"); f.hdr.Typeflag != tar.TypeDir || f.hdr.Mode != 0o700 {
		t.Errorf("etc type %q mode %o", f.hdr.Typeflag, f.hdr.Mode)
	}
}

func TestDiskAWhiteoutTakesTheSubtree(t *testing.T) {
	base := layerOf(t, dir("opt/"), dir("opt/tool/"), reg("opt/tool/a", "a"))
	top := layerOf(t, reg("opt/.wh.tool", ""))

	got := stream(t, base, top)

	missing(t, got, "opt/tool")
	missing(t, got, "opt/tool/a")
	file(t, got, "opt")
}

// A hard link to a target that keeps its name stays a link, so the image holds the body once.
func TestDiskAHardLinkStaysALink(t *testing.T) {
	hard := entry{hdr: tar.Header{Name: "bin/ls", Typeflag: tar.TypeLink, Linkname: "bin/busybox"}}

	got := stream(t, layerOf(t, dir("bin/"), reg("bin/busybox", "bb"), hard))

	if f := file(t, got, "bin/ls"); f.hdr.Typeflag != tar.TypeLink || f.hdr.Linkname != "bin/busybox" {
		t.Errorf("bin/ls type %q -> %s", f.hdr.Typeflag, f.hdr.Linkname)
	}
}

// A hard link made in a lower layer keeps the content it linked, even when a higher layer replaces the target.
func TestDiskAHardLinkKeepsTheVersionItTook(t *testing.T) {
	hard := entry{hdr: tar.Header{Name: "bin/ls", Typeflag: tar.TypeLink, Linkname: "bin/busybox"}}
	base := layerOf(t, dir("bin/"), reg("bin/busybox", "old-busybox"), hard)
	top := layerOf(t, reg("bin/busybox", "new"))

	got := stream(t, base, top)

	if body := bodyOf(t, got, "bin/ls"); body != "old-busybox" {
		t.Errorf("bin/ls is %q", body)
	}
	if body := bodyOf(t, got, "bin/busybox"); body != "new" {
		t.Errorf("bin/busybox is %q", body)
	}
}

// A whiteout of the target leaves the content to its link alone: no name of the target comes back.
func TestDiskAWhiteoutOfALinkTargetKeepsOnlyTheLink(t *testing.T) {
	alias := entry{hdr: tar.Header{Name: "bin/alias", Typeflag: tar.TypeLink, Linkname: "bin/tool"}}
	base := layerOf(t, dir("bin/"), reg("bin/tool", "tool-body"), alias)
	top := layerOf(t, reg("bin/.wh.tool", ""))

	got := stream(t, base, top)

	if body := bodyOf(t, got, "bin/alias"); body != "tool-body" {
		t.Errorf("bin/alias is %q", body)
	}
	missing(t, got, "bin/tool")
}

// Two links to a lost target share one body: the first carries it, the second links to the first.
func TestDiskTwoLinksToALostTargetShareOneBody(t *testing.T) {
	alias := entry{hdr: tar.Header{Name: "bin/alias", Typeflag: tar.TypeLink, Linkname: "bin/tool"}}
	other := entry{hdr: tar.Header{Name: "sbin/other", Typeflag: tar.TypeLink, Linkname: "bin/tool"}}
	base := layerOf(t, dir("bin/"), reg("bin/tool", "tool-body"), alias, dir("sbin/"), other)
	top := layerOf(t, reg("bin/.wh.tool", ""))

	got := stream(t, base, top)

	if body := bodyOf(t, got, "bin/alias"); body != "tool-body" {
		t.Errorf("bin/alias is %q", body)
	}
	if f := file(t, got, "sbin/other"); f.hdr.Typeflag != tar.TypeLink || f.hdr.Linkname != "bin/alias" {
		t.Errorf("sbin/other type %q -> %s", f.hdr.Typeflag, f.hdr.Linkname)
	}
	missing(t, got, "bin/tool")
}

// A target whose every link is gone is gone too, whatever a lower layer linked to it.
func TestDiskAWhiteoutOfTheLinkAndTheTargetDropsBoth(t *testing.T) {
	alias := entry{hdr: tar.Header{Name: "bin/alias", Typeflag: tar.TypeLink, Linkname: "bin/tool"}}
	base := layerOf(t, dir("bin/"), reg("bin/tool", "tool-body"), alias)
	top := layerOf(t, reg("bin/.wh.tool", ""), reg("bin/.wh.alias", ""))

	got := stream(t, base, top)

	missing(t, got, "bin/tool")
	missing(t, got, "bin/alias")
}

// A link to a link reaches the file behind both, even once the two earlier names are gone.
func TestDiskALinkToALinkOutlivesBothEarlierNames(t *testing.T) {
	first := entry{hdr: tar.Header{Name: "bin/first", Typeflag: tar.TypeLink, Linkname: "bin/tool"}}
	last := entry{hdr: tar.Header{Name: "bin/last", Typeflag: tar.TypeLink, Linkname: "bin/first"}}
	base := layerOf(t, dir("bin/"), reg("bin/tool", "tool-body"), first)
	mid := layerOf(t, last)
	top := layerOf(t, reg("bin/.wh.tool", ""), reg("bin/.wh.first", ""))

	got := stream(t, base, mid, top)

	if body := bodyOf(t, got, "bin/last"); body != "tool-body" {
		t.Errorf("bin/last is %q", body)
	}
	missing(t, got, "bin/tool")
	missing(t, got, "bin/first")
}

func TestDiskRefusesWhatTheGuestCannotHold(t *testing.T) {
	odd := entry{hdr: tar.Header{Name: "odd", Typeflag: 'Z', Mode: 0o644}}

	path := filepath.Join(t.TempDir(), "rootfs.ext4")
	err := buildDisk(t.Context(), path, []v1.Layer{layerOf(t, odd)})
	if err == nil || !strings.Contains(err.Error(), `tar type 'Z'`) {
		t.Fatalf("buildDisk: %v", err)
	}
}

// The image the stream lands in is ext4 the host's fsck accepts, where the host has one.
func TestDiskBuildsAnImageFsckAccepts(t *testing.T) {
	hard := entry{hdr: tar.Header{Name: "bin/ls", Typeflag: tar.TypeLink, Linkname: "bin/busybox"}}
	base := layerOf(t, dir("bin/"), reg("bin/busybox", "old-busybox"), hard, reg("home/app/.profile", "x"))
	top := layerOf(t, reg("bin/busybox", "new"), reg("bin/.wh.ls", ""))

	path := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := buildDisk(t.Context(), path, []v1.Layer{base, top}); err != nil {
		t.Fatalf("buildDisk: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	magic := make([]byte, 2)
	if _, err := f.ReadAt(magic, 1024+0x38); err != nil {
		t.Fatal(err)
	}
	if magic[0] != 0x53 || magic[1] != 0xef {
		t.Fatalf("magic %x", magic)
	}
	if _, err := exec.LookPath("e2fsck"); err != nil {
		t.Logf("no e2fsck on PATH, the magic stands alone")

		return
	}
	out, err := exec.Command("e2fsck", "-fn", path).CombinedOutput()
	if err != nil {
		t.Fatalf("e2fsck: %v\n%s", err, out)
	}
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
