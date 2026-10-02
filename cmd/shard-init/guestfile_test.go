package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/presmihaylov/shard/services/supervisor"
)

// full stands in for a guest disk with no block left, where a new temp file fails.
func full(path string, _ []byte, _ fs.FileMode) error {
	return &fs.PathError{Op: "write", Path: path + ".tmp", Err: syscall.ENOSPC}
}

func writeFile(t *testing.T, path, content string, perm fs.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return string(got)
}

// A fork of a full sandbox gets a new address, so its resolv.conf changes on a disk with no block left.
func TestAFullDiskRewritesAFileInTheBlocksItHolds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolv.conf")
	writeFile(t, path, "nameserver 10.200.0.1\n", 0o600)

	if err := writeOnFullDisk(full, path, []byte("nameserver 10.200.0.9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "nameserver 10.200.0.9\n" {
		t.Errorf("resolv.conf = %q", got)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("resolv.conf mode = %v, %v; want 0644", info.Mode(), err)
	}
}

// A crash during an in-place write leaves part of the file, and the next start writes it whole.
func TestAPartialFileFromACrashIsRewrittenByTheNextStart(t *testing.T) {
	a := supervisor.Address{IP: "10.200.0.2", Hostname: "sb-1", Nameservers: []string{"10.200.0.1"}}

	t.Run("a disk with room", func(t *testing.T) {
		etc := t.TempDir()
		writeFile(t, filepath.Join(etc, "resolv.conf"), "nameser", 0o644)
		if err := writeResolverFilesIn(etc, a); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, filepath.Join(etc, "resolv.conf")); got != "nameserver 10.200.0.1\n" {
			t.Errorf("resolv.conf = %q", got)
		}
	})
	t.Run("a full disk", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "resolv.conf")
		writeFile(t, path, "nameser", 0o644)
		if err := writeOnFullDisk(full, path, []byte("nameserver 10.200.0.1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, path); got != "nameserver 10.200.0.1\n" {
			t.Errorf("resolv.conf = %q", got)
		}
	})
}

// A start that changes nothing writes nothing, so a guest root that takes no new file still starts.
func TestAStartWritesNothingWhenTheFilesHoldTheContent(t *testing.T) {
	etc := t.TempDir()
	a := supervisor.Address{IP: "10.200.0.2", Hostname: "sb-1", Nameservers: []string{"10.200.0.1"}}
	if err := writeResolverFilesIn(etc, a); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(etc, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(etc, 0o755); err != nil {
			t.Error(err)
		}
	})

	if err := writeResolverFilesIn(etc, a); err != nil {
		t.Fatalf("a start with the same address = %v, want no write at all", err)
	}
}

// Only a full disk earns the in-place write, and only into a regular file of one name whose blocks fit the new bytes.
func TestAFullDiskKeepsTheRefusalWhenTheFileHasNoRoom(t *testing.T) {
	denied := func(path string, _ []byte, _ fs.FileMode) error {
		return &fs.PathError{Op: "open", Path: path, Err: syscall.EACCES}
	}
	cases := map[string]struct {
		replace func(string, []byte, fs.FileMode) error
		lay     func(t *testing.T, path string)
		full    bool
	}{
		"an empty file holds no block": {replace: full, full: true, lay: func(t *testing.T, path string) {
			writeFile(t, path, "", 0o644)
		}},
		"a symlink is never followed": {replace: full, full: true, lay: func(t *testing.T, path string) {
			writeFile(t, path+".target", "nameserver 10.200.0.1\n", 0o644)
			if err := os.Symlink(path+".target", path); err != nil {
				t.Fatal(err)
			}
		}},
		"a fifo is never waited on": {replace: full, full: true, lay: func(t *testing.T, path string) {
			if err := syscall.Mkfifo(path, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		"a hard link is never shared": {replace: full, full: true, lay: func(t *testing.T, path string) {
			writeFile(t, path+".user", "the user wrote this file\n", 0o644)
			if err := os.Link(path+".user", path); err != nil {
				t.Fatal(err)
			}
		}},
		"another error than a full disk": {replace: denied, lay: func(t *testing.T, path string) {
			writeFile(t, path, "nameserver 10.200.0.1\n", 0o644)
		}},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "resolv.conf")
			c.lay(t, path)
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}

			err = writeOnFullDisk(c.replace, path, []byte("nameserver 10.200.0.9\n"), 0o644)
			if err == nil {
				t.Fatal("the write succeeded")
			}
			if c.full && !strings.Contains(err.Error(), "the guest disk is full: free space in the source sandbox") {
				t.Errorf("the error does not name the full disk: %v", err)
			}
			if !c.full && !errors.Is(err, syscall.EACCES) {
				t.Errorf("the error is not the one replace gave: %v", err)
			}
			after, err := os.Lstat(path)
			if err != nil || after.Mode() != before.Mode() || after.Size() != before.Size() {
				t.Errorf("the file changed from %v to %v, %v", before, after, err)
			}
		})
	}
}
