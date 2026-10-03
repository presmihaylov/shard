package tarball_test

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/pkg/tarball"
)

// entry is one tar entry a test builds by hand, as a hostile guest would.
type entry struct {
	name string
	typ  byte
	body string
	link string
	mode int64
}

func archive(t *testing.T, entries ...entry) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Linkname: e.link, Mode: mode, Size: int64(len(e.body))}
		if e.typ != tar.TypeReg {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write the header of %s: %v", e.name, err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatalf("write the body of %s: %v", e.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close the tar: %v", err)
	}

	return &buf
}

// sandbox answers an empty dst and a sibling outside it that holds one file, so a test sees any write that escapes.
func sandbox(t *testing.T) (dst, outside string) {
	t.Helper()

	parent := t.TempDir()
	dst = filepath.Join(parent, "dst")
	outside = filepath.Join(parent, "outside")
	for _, dir := range []string{dst, outside} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatalf("make %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("host"), 0o600); err != nil {
		t.Fatalf("write the outside file: %v", err)
	}

	return dst, outside
}

// untouched fails the test when anything under outside changed, or when anything landed beside dst.
func untouched(t *testing.T, dst, outside string) {
	t.Helper()

	names, err := os.ReadDir(outside)
	if err != nil {
		t.Fatalf("read the outside dir: %v", err)
	}
	if len(names) != 1 || names[0].Name() != "secret" {
		t.Fatalf("the unpack wrote outside dst: %v", names)
	}
	body, err := os.ReadFile(filepath.Join(outside, "secret"))
	if err != nil || string(body) != "host" {
		t.Fatalf("the outside file now reads %q, %v", body, err)
	}
	siblings, err := os.ReadDir(filepath.Dir(dst))
	if err != nil {
		t.Fatalf("read the parent dir: %v", err)
	}
	if len(siblings) != 2 {
		t.Fatalf("the unpack wrote beside dst: %v", siblings)
	}
}

func refusedAt(t *testing.T, err error, name string) {
	t.Helper()

	var refused *tarball.RefusedError
	if !errors.As(err, &refused) || refused.Name != name {
		t.Fatalf("the unpack answered %v, want a refusal of %q", err, name)
	}
}

func TestUnpackRefusesAHostileArchive(t *testing.T) {
	cases := []struct {
		name    string
		opts    tarball.Options
		entries []entry
		refused string
	}{
		{name: "an absolute name", entries: []entry{{name: "/etc/x", typ: tar.TypeReg, body: "x"}}, refused: "/etc/x"},
		{name: "a ../ entry", entries: []entry{{name: "../outside/x", typ: tar.TypeReg, body: "x"}}, refused: "../outside/x"},
		{name: "a .. inside a name", entries: []entry{{name: "a/../../outside/x", typ: tar.TypeReg, body: "x"}}, refused: "a/../../outside/x"},
		{name: "an empty name", entries: []entry{{name: "", typ: tar.TypeReg}}, refused: ""},
		{name: "a symlink out", opts: tarball.Options{ConfineLinks: true}, entries: []entry{{name: "l", typ: tar.TypeSymlink, link: "../outside"}}, refused: "l"},
		{name: "an absolute symlink", opts: tarball.Options{ConfineLinks: true}, entries: []entry{{name: "l", typ: tar.TypeSymlink, link: "/etc"}}, refused: "l"},
		{
			name:    "an entry under a symlink out",
			entries: []entry{{name: "l", typ: tar.TypeSymlink, link: "../outside"}, {name: "l/x", typ: tar.TypeReg, body: "x"}},
			refused: "l/x",
		},
		{
			name:    "an entry under an absolute symlink",
			entries: []entry{{name: "l", typ: tar.TypeSymlink, link: "/"}, {name: "l/x", typ: tar.TypeReg, body: "x"}},
			refused: "l/x",
		},
		{
			name:    "an entry under a chain of links out",
			entries: []entry{{name: "x", typ: tar.TypeSymlink, link: "."}, {name: "a", typ: tar.TypeSymlink, link: "x/../outside"}, {name: "a/y", typ: tar.TypeReg, body: "y"}},
			refused: "a/y",
		},
		{
			name:    "a chain of links out",
			opts:    tarball.Options{ConfineLinks: true},
			entries: []entry{{name: "x", typ: tar.TypeSymlink, link: "."}, {name: "a", typ: tar.TypeSymlink, link: "x/../outside"}},
			refused: "a",
		},
		{name: "a hard link out", entries: []entry{{name: "h", typ: tar.TypeLink, link: "../outside/secret"}}, refused: "h"},
		{name: "a hard link to a file not in the archive", entries: []entry{{name: "h", typ: tar.TypeLink, link: "secret"}}, refused: "h"},
		{
			name:    "a hard link to a name a symlink took over",
			entries: []entry{{name: "f", typ: tar.TypeReg, body: "x"}, {name: "f", typ: tar.TypeSymlink, link: "g"}, {name: "h", typ: tar.TypeLink, link: "f"}},
			refused: "h",
		},
		{name: "a char device", entries: []entry{{name: "null", typ: tar.TypeChar}}, refused: "null"},
		{name: "a block device", entries: []entry{{name: "sda", typ: tar.TypeBlock}}, refused: "sda"},
		{name: "a fifo", entries: []entry{{name: "pipe", typ: tar.TypeFifo}}, refused: "pipe"},
		{name: "a name outside strip", opts: tarball.Options{Strip: "app"}, entries: []entry{{name: "other/x", typ: tar.TypeReg, body: "x"}}, refused: "other/x"},
		{name: "a file at the destination itself", opts: tarball.Options{Strip: "app"}, entries: []entry{{name: "app", typ: tar.TypeReg, body: "x"}}, refused: "app"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dst, outside := sandbox(t)

			refusedAt(t, tarball.Unpack(archive(t, c.entries...), dst, c.opts), c.refused)
			untouched(t, dst, outside)
		})
	}
}

// A refused chain leaves no link behind, so nothing that later follows the tree reaches the host.
func TestUnpackRemovesALinkThatLeavesThroughAnother(t *testing.T) {
	dst, outside := sandbox(t)

	err := tarball.Unpack(archive(t, entry{name: "x", typ: tar.TypeSymlink, link: "."}, entry{name: "a", typ: tar.TypeSymlink, link: "x/../outside"}), dst, tarball.Options{ConfineLinks: true})
	refusedAt(t, err, "a")
	if _, err := os.Lstat(filepath.Join(dst, "a")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the refused link is still there: %v", err)
	}
	untouched(t, dst, outside)
}

// On a filesystem that folds case, two names that differ only in case are one, so the second must not reach through the first.
func TestUnpackOfNamesThatDifferInCaseStaysInside(t *testing.T) {
	for _, opts := range []tarball.Options{{}, {ConfineLinks: true}} {
		t.Run(fmt.Sprintf("confine %t", opts.ConfineLinks), func(t *testing.T) {
			dst, outside := sandbox(t)

			// A folding filesystem refuses the second entry, a case-sensitive one lands it in its own directory; either way nothing leaves dst.
			err := tarball.Unpack(archive(t, entry{name: "a", typ: tar.TypeSymlink, link: "../outside"}, entry{name: "A/secret", typ: tar.TypeReg, body: "guest"}), dst, opts)
			var refused *tarball.RefusedError
			if err != nil && !errors.As(err, &refused) {
				t.Fatalf("the unpack failed with %v, want a refusal or none", err)
			}
			untouched(t, dst, outside)
		})
	}
}

// A folding filesystem lets a symlink spelled A take the name of the file a, so a hard link to a must not copy that symlink past confinement.
func TestUnpackOfAHardLinkToACaseFoldedNameStaysInside(t *testing.T) {
	dst, outside := sandbox(t)

	// A case-sensitive filesystem refuses A as a link out; a folding one refuses h, whose name A took over.
	err := tarball.Unpack(archive(t,
		entry{name: "x", typ: tar.TypeSymlink, link: "."},
		entry{name: "a", typ: tar.TypeReg, body: "guest"},
		entry{name: "A", typ: tar.TypeSymlink, link: "x/../outside"},
		entry{name: "h", typ: tar.TypeLink, link: "a"},
	), dst, tarball.Options{ConfineLinks: true})
	var refused *tarball.RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("the unpack answered %v, want a refusal", err)
	}
	untouched(t, dst, outside)
	staysInside(t, dst)
}

// swapReader runs swap just before the tar reader reads the byte at offset at.
type swapReader struct {
	r    *bytes.Reader
	at   int64
	swap func() error
	done bool
}

func (s *swapReader) Read(p []byte) (int, error) {
	off := s.r.Size() - int64(s.r.Len())
	if !s.done && off+int64(len(p)) > s.at {
		s.done = true
		if err := s.swap(); err != nil {
			return 0, err
		}
	}

	return s.r.Read(p)
}

// Whatever takes the name of a file the archive wrote before its hard link arrives, the link refuses rather than copy it.
func TestUnpackRefusesAHardLinkWhoseTargetWasReplaced(t *testing.T) {
	dst, outside := sandbox(t)

	data := archive(t, entry{name: "a", typ: tar.TypeReg, body: "guest"}, entry{name: "h", typ: tar.TypeLink, link: "a"}).Bytes()
	// The header of a, then its one block of body.
	at := int64(2 * 512)
	if string(data[at:at+2]) != "h\x00" {
		t.Fatalf("the header at %d is not h", at)
	}
	swap := func() error {
		target := filepath.Join(dst, "a")
		if err := os.Remove(target); err != nil {
			return err
		}

		return os.Symlink("../outside/secret", target)
	}

	err := tarball.Unpack(&swapReader{r: bytes.NewReader(data), at: at, swap: swap}, dst, tarball.Options{})
	refusedAt(t, err, "h")
	names, err := os.ReadDir(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if len(names) != 1 || names[0].Name() != "a" {
		t.Fatalf("the refused link left %v in dst, want only a", names)
	}
	untouched(t, dst, outside)
}

// staysInside fails the test when a symlink left under dst resolves outside it.
func staysInside(t *testing.T, dst string) {
	t.Helper()

	root, err := filepath.EvalSymlinks(dst)
	if err != nil {
		t.Fatalf("resolve dst: %v", err)
	}
	walk := filepath.WalkDir(dst, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink == 0 {
			return err
		}
		resolved, err := filepath.EvalSymlinks(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
			t.Errorf("%s resolves to %s, outside dst", path, resolved)
		}

		return nil
	})
	if walk != nil {
		t.Fatalf("walk dst: %v", walk)
	}
}

func TestUnpackCapsTheArchive(t *testing.T) {
	cases := []struct {
		name    string
		opts    tarball.Options
		refused string
	}{
		{name: "bytes", opts: tarball.Options{MaxBytes: 10}, refused: "b"},
		{name: "entries", opts: tarball.Options{MaxEntries: 2}, refused: "c"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dst, outside := sandbox(t)

			err := tarball.Unpack(archive(t, entry{name: "a", typ: tar.TypeReg, body: "123456"}, entry{name: "b", typ: tar.TypeReg, body: "123456"}, entry{name: "c", typ: tar.TypeDir}), dst, c.opts)
			refusedAt(t, err, c.refused)
			untouched(t, dst, outside)
		})
	}
}

func TestUnpackRefusesWhatIsNotATar(t *testing.T) {
	dst, _ := sandbox(t)

	err := tarball.Unpack(strings.NewReader(strings.Repeat("not a tar ", 100)), dst, tarball.Options{})
	if !errors.Is(err, tar.ErrHeader) {
		t.Fatalf("the unpack answered %v, want tar.ErrHeader", err)
	}
}

func TestUnpackKeepsSetidOnlyWhenAsked(t *testing.T) {
	for _, keep := range []bool{false, true} {
		t.Run(fmt.Sprintf("keep %t", keep), func(t *testing.T) {
			dst, _ := sandbox(t)

			if err := tarball.Unpack(archive(t, entry{name: "su", typ: tar.TypeReg, body: "x", mode: 0o4755}), dst, tarball.Options{KeepSetid: keep}); err != nil {
				t.Fatalf("unpack: %v", err)
			}
			info, err := os.Stat(filepath.Join(dst, "su"))
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			want := fs.FileMode(0o755)
			if keep {
				want |= fs.ModeSetuid
			}
			if info.Mode() != want {
				t.Fatalf("the file has mode %v, want %v", info.Mode(), want)
			}
		})
	}
}

// A file entry replaces a symlink already at its name and never writes through it.
func TestUnpackReplacesALinkAtAFileName(t *testing.T) {
	dst, outside := sandbox(t)
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dst, "f")); err != nil {
		t.Fatalf("plant the link: %v", err)
	}

	if err := tarball.Unpack(archive(t, entry{name: "f", typ: tar.TypeReg, body: "guest"}), dst, tarball.Options{}); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	info, err := os.Lstat(filepath.Join(dst, "f"))
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("dst/f is %v, %v, want a regular file", info, err)
	}
	untouched(t, dst, outside)
}

func TestUnpackLeavesAnExistingDirectorysMode(t *testing.T) {
	dst, _ := sandbox(t)
	if err := os.Mkdir(filepath.Join(dst, "srv"), 0o700); err != nil {
		t.Fatalf("make srv: %v", err)
	}

	if err := tarball.Unpack(archive(t, entry{name: "srv", typ: tar.TypeDir, mode: 0o755}, entry{name: "srv/f", typ: tar.TypeReg, body: "x"}), dst, tarball.Options{}); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	info, err := os.Stat(filepath.Join(dst, "srv"))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("srv is %v, %v, want it left at 0700", info, err)
	}
}

// A directory the tar makes read-only still takes its entries, since its mode lands only once the tree is in.
func TestUnpackSetsADirectorysModeAfterItsEntries(t *testing.T) {
	dst, _ := sandbox(t)
	t.Cleanup(func() {
		if err := os.Chmod(filepath.Join(dst, "ro"), 0o755); err != nil {
			t.Errorf("open ro back up: %v", err)
		}
	})

	if err := tarball.Unpack(archive(t, entry{name: "ro/", typ: tar.TypeDir, mode: 0o555}, entry{name: "ro/f", typ: tar.TypeReg, body: "x"}), dst, tarball.Options{}); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	info, err := os.Stat(filepath.Join(dst, "ro"))
	if err != nil || info.Mode().Perm() != 0o555 {
		t.Fatalf("ro is %v, %v, want 0555", info, err)
	}
}

// A pack and an unpack give back the same tree: every file's bytes and mode, and every symlink's target.
func TestPackThenUnpackKeepsTheTree(t *testing.T) {
	src := filepath.Join(t.TempDir(), "app")
	tree := map[string]string{"run.sh": "#!/bin/sh\n", "conf/app.conf": "port=1", "conf/secret": "s", "data/big": strings.Repeat("b", 1<<20)}
	for name, body := range tree {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(src, name)), 0o755); err != nil {
			t.Fatalf("make the parent of %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	for name, mode := range map[string]fs.FileMode{"run.sh": 0o755, "conf/secret": 0o600, "conf": 0o750} {
		if err := os.Chmod(filepath.Join(src, name), mode); err != nil {
			t.Fatalf("chmod %s: %v", name, err)
		}
	}
	for name, target := range map[string]string{"current": "data/big", "dangling": "nowhere", "conf/up": "../run.sh"} {
		if err := os.Symlink(target, filepath.Join(src, name)); err != nil {
			t.Fatalf("link %s: %v", name, err)
		}
	}
	if err := os.Mkdir(filepath.Join(src, "empty"), 0o700); err != nil {
		t.Fatalf("make empty: %v", err)
	}

	var buf bytes.Buffer
	if err := tarball.Pack(&buf, src, "app"); err != nil {
		t.Fatalf("pack: %v", err)
	}
	dst, _ := sandbox(t)
	if err := tarball.Unpack(&buf, dst, tarball.Options{Strip: "app", ConfineLinks: true}); err != nil {
		t.Fatalf("unpack: %v", err)
	}

	want, got := shape(t, src), shape(t, dst)
	if len(want) != 10 || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the tree came back as\n%v\nwant\n%v", got, want)
	}
}

// shape answers one line per path under dir: its type and mode, and its hash or its link target.
func shape(t *testing.T, dir string) []string {
	t.Helper()

	var lines []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		line := fmt.Sprintf("%s %v", rel, info.Mode())
		switch {
		case info.Mode().IsRegular():
			body, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			line += fmt.Sprintf(" %x", sha256.Sum256(body))
		case info.Mode().Type() == fs.ModeSymlink:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			line += " -> " + target
		}
		lines = append(lines, line)

		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}

	return lines
}

func TestUnpackHardLinksAFileEarlierInTheArchive(t *testing.T) {
	dst, _ := sandbox(t)

	if err := tarball.Unpack(archive(t, entry{name: "a", typ: tar.TypeReg, body: "same"}, entry{name: "dir/b", typ: tar.TypeLink, link: "a"}), dst, tarball.Options{}); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	a, errA := os.Stat(filepath.Join(dst, "a"))
	b, errB := os.Stat(filepath.Join(dst, "dir/b"))
	if errA != nil || errB != nil || !os.SameFile(a, b) {
		t.Fatalf("dir/b is not a hard link of a: %v, %v", errA, errB)
	}
}
