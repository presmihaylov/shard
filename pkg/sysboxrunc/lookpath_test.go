package sysboxrunc_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/presmihaylov/shard/pkg/sysboxrunc"
)

// rootfs lays out a small guest tree: /usr/bin/sh, /bin -> usr/bin, /sbin -> /usr/sbin (absent),
// /etc/passwd without an execute bit, /opt as a directory, and a symlink loop at /loop.
func rootfs(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	writeExecutable(t, filepath.Join(root, "usr", "bin", "sh"))
	writeExecutable(t, filepath.Join(root, "usr", "local", "bin", "tool"))

	if err := os.WriteFile(filepath.Join(root, "usr", "bin", "data"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write data: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "opt"), 0o755); err != nil {
		t.Fatalf("mkdir opt: %v", err)
	}

	links := map[string]string{
		"bin":       "usr/bin",
		"sbin":      "/usr/sbin",
		"loop":      "/loop",
		"local":     "/usr/local",
		"stale":     "/nowhere/at/all",
		"usr/bin/x": "sh",
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatalf("link %s: %v", name, err)
		}
	}

	return root
}

func TestLookPathAnswersLikeAShell(t *testing.T) {
	root := rootfs(t)

	cases := []struct {
		name          string
		command       string
		workDir       string
		path          string
		reason        string
		notExecutable bool
	}{
		{name: "an absolute path that is there", command: "/usr/bin/sh"},
		{name: "through a relative symlinked directory", command: "/bin/sh"},
		{name: "through an absolute symlinked directory", command: "/local/bin/tool"},
		{name: "a relative symlink to a file", command: "/bin/x"},
		{name: "a bare name on PATH", command: "sh", path: "/nowhere:/bin"},
		{name: "a bare name further down PATH", command: "tool", path: "/bin:/usr/local/bin"},
		{name: "a relative path from the cwd", command: "bin/sh", workDir: "/usr"},
		{name: "a dot-relative path", command: "./sh", workDir: "/usr/bin"},
		{name: "a missing file", command: "/usr/bin/nope", reason: "/usr/bin/nope: not found"},
		{name: "a missing directory on the way", command: "/nope/sh", reason: "/nope/sh: not found"},
		{name: "a symlink to a directory that is not there", command: "/sbin/init", reason: "/sbin/init: not found"},
		{name: "a dangling symlink", command: "/stale", reason: "/stale: not found"},
		{name: "a bare name nowhere on PATH", command: "nope", path: "/bin:/usr/bin", reason: "nope: not found"},
		{name: "a bare name with an empty PATH", command: "sh", reason: "sh: not found"},
		{name: "a file with no execute bit", command: "/usr/bin/data", reason: "/usr/bin/data: permission denied", notExecutable: true},
		{name: "a directory", command: "/opt", reason: "/opt: is a directory", notExecutable: true},
		{name: "a bare name that matches only a file that cannot run", command: "data", path: "/bin:/usr/bin", reason: "data: permission denied", notExecutable: true},
		{name: "a symlink loop", command: "/loop/sh", reason: "/loop/sh: too many levels of symbolic links"},
		{name: "an empty command", command: "", reason: "empty command"},
		{name: "a climb out of the root stays inside it", command: "/../../usr/bin/sh"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := sysboxrunc.LookPath(root, tc.workDir, tc.path, tc.command)
			if tc.reason == "" {
				if err != nil {
					t.Fatalf("LookPath refused %q: %v", tc.command, err)
				}

				return
			}

			var lookup *sysboxrunc.LookupError
			if !errors.As(err, &lookup) {
				t.Fatalf("LookPath returned %v for %q, want a LookupError", err, tc.command)
			}
			if lookup.Reason != tc.reason {
				t.Errorf("the reason is %q, want %q", lookup.Reason, tc.reason)
			}
			if lookup.NotExecutable != tc.notExecutable {
				t.Errorf("NotExecutable is %v, want %v", lookup.NotExecutable, tc.notExecutable)
			}
		})
	}
}

// The symlinks resolve against the guest tree and never the host, or /bin -> /usr/bin would land in
// the host's /usr/bin and approve a command the container does not have.
func TestLookPathNeverLeavesTheRootFS(t *testing.T) {
	root := rootfs(t)

	// /bin/ls exists on every host that runs this test, and the guest's /usr/bin has no ls.
	err := sysboxrunc.LookPath(root, "", "", "/bin/ls")

	var lookup *sysboxrunc.LookupError
	if !errors.As(err, &lookup) || lookup.Reason != "/bin/ls: not found" {
		t.Fatalf("LookPath returned %v for a command only the host has, want not found", err)
	}
}
