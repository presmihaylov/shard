package xfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFstabAddsTheLineOnce(t *testing.T) {
	FstabPath = filepath.Join(t.TempDir(), "fstab")
	if err := os.WriteFile(FstabPath, []byte("# static\n/dev/sda1 / ext4 defaults 0 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if err := Fstab("/var/lib/shard.xfs", "/var/lib/shard"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(FstabPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := "/var/lib/shard.xfs /var/lib/shard xfs loop,nofail 0 0\n"; strings.Count(string(got), want) != 1 || !strings.HasPrefix(string(got), "# static\n") {
		t.Errorf("fstab holds:\n%s", got)
	}
}

// TestFstabKeepsATailWithNoNewlineOnItsOwnLine covers a host fstab whose last line has no trailing newline.
func TestFstabKeepsATailWithNoNewlineOnItsOwnLine(t *testing.T) {
	FstabPath = filepath.Join(t.TempDir(), "fstab")
	if err := os.WriteFile(FstabPath, []byte("/dev/sda1 / ext4 defaults 0 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Fstab("/var/lib/shard.xfs", "/var/lib/shard"); err != nil {
		t.Fatal(err)
	}

	want := "/dev/sda1 / ext4 defaults 0 1\n/var/lib/shard.xfs /var/lib/shard xfs loop,nofail 0 0\n"
	got, err := os.ReadFile(FstabPath)
	if err != nil || string(got) != want {
		t.Errorf("fstab holds %q, %v", got, err)
	}
}

// TestFstabEscapesASpaceAndStaysIdempotent covers a root path with a space, which must escape and still add once.
func TestFstabEscapesASpaceAndStaysIdempotent(t *testing.T) {
	FstabPath = filepath.Join(t.TempDir(), "fstab")
	if err := os.WriteFile(FstabPath, []byte("# static\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if err := Fstab("/var/lib/my data.xfs", "/var/lib/my data"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(FstabPath)
	if err != nil {
		t.Fatal(err)
	}
	want := `/var/lib/my\040data.xfs /var/lib/my\040data xfs loop,nofail 0 0` + "\n"
	if strings.Count(string(got), want) != 1 {
		t.Errorf("fstab holds:\n%s", got)
	}
}

func TestFstabNeedsTheFile(t *testing.T) {
	FstabPath = filepath.Join(t.TempDir(), "fstab")
	if err := Fstab("/x.xfs", "/x"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing fstab got %v", err)
	}
}

// TestFstabUpgradesAnOldLineInPlace covers a host whose fstab still holds the pre-nofail line; Fstab adds nofail without a duplicate, leaves the other lines alone, and stays idempotent.
func TestFstabUpgradesAnOldLineInPlace(t *testing.T) {
	FstabPath = filepath.Join(t.TempDir(), "fstab")
	old := "# static\n/dev/sda1 / ext4 defaults 0 1\n/var/lib/shard.xfs /var/lib/shard xfs loop 0 0\n"
	if err := os.WriteFile(FstabPath, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if err := Fstab("/var/lib/shard.xfs", "/var/lib/shard"); err != nil {
			t.Fatal(err)
		}
	}
	want := "# static\n/dev/sda1 / ext4 defaults 0 1\n/var/lib/shard.xfs /var/lib/shard xfs loop,nofail 0 0\n"
	got, err := os.ReadFile(FstabPath)
	if err != nil || string(got) != want {
		t.Errorf("fstab holds %q, %v; want %q", got, err, want)
	}
}

// TestMigrateFstabRepairsAnOldLineOnly covers the running-host path: an old line gains nofail, and anything else stays untouched.
func TestMigrateFstabRepairsAnOldLineOnly(t *testing.T) {
	const image, point = "/var/lib/shard.xfs", "/var/lib/shard"
	old := "/dev/sda1 / ext4 defaults 0 1\n/var/lib/shard.xfs /var/lib/shard xfs loop 0 0\n"
	upgraded := "/dev/sda1 / ext4 defaults 0 1\n/var/lib/shard.xfs /var/lib/shard xfs loop,nofail 0 0\n"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"an old line gains nofail", old, upgraded},
		{"a line with nofail is left alone", upgraded, upgraded},
		{"a foreign mount at the point is left alone", "/dev/sdb1 /var/lib/shard btrfs defaults 0 0\n", "/dev/sdb1 /var/lib/shard btrfs defaults 0 0\n"},
		{"no line at the point adds nothing", "# static\n", "# static\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			FstabPath = filepath.Join(t.TempDir(), "fstab")
			if err := os.WriteFile(FstabPath, []byte(c.in), 0o644); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := MigrateFstab(image, point); err != nil {
					t.Fatal(err)
				}
			}
			got, err := os.ReadFile(FstabPath)
			if err != nil || string(got) != c.want {
				t.Errorf("fstab holds %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

func TestMigrateFstabIgnoresAMissingFile(t *testing.T) {
	FstabPath = filepath.Join(t.TempDir(), "fstab")
	if err := MigrateFstab("/x.xfs", "/x"); err != nil {
		t.Errorf("a missing fstab got %v", err)
	}
}

func TestIsImageReadsTheSuperblockMagic(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	image := filepath.Join(dir, "a.xfs")
	if err := os.WriteFile(image, append([]byte(magic), make([]byte, 508)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := IsImage(image); err != nil || !ok {
		t.Errorf("a superblock got %v, %v", ok, err)
	}

	if ok, err := IsImage(filepath.Join(dir, "missing")); err != nil || ok {
		t.Errorf("a missing file got %v, %v", ok, err)
	}

	other := filepath.Join(dir, "other")
	if err := os.WriteFile(other, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := IsImage(other); !errors.Is(err, ErrNotImage) {
		t.Errorf("another file got %v", err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := IsImage(empty); !errors.Is(err, ErrNotImage) {
		t.Errorf("an empty file got %v", err)
	}
}

func TestFstabRefusesAForeignLineAtThePoint(t *testing.T) {
	FstabPath = filepath.Join(t.TempDir(), "fstab")
	for _, line := range []string{
		"/dev/sdb1 /var/lib/shard ext4 defaults 0 2",
		"/other.xfs /var/lib/shard xfs loop 0 0",
		"/var/lib/shard.xfs /var/lib/shard xfs defaults 0 0",
	} {
		if err := os.WriteFile(FstabPath, []byte(line+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := Fstab("/var/lib/shard.xfs", "/var/lib/shard")
		if !errors.Is(err, ErrFstabConflict) || !strings.Contains(err.Error(), line) {
			t.Errorf("%q got %v", line, err)
		}
		got, err := os.ReadFile(FstabPath)
		if err != nil || string(got) != line+"\n" {
			t.Errorf("after the conflict fstab holds %q, %v", got, err)
		}
	}
}
