//go:build integration

package cli

import (
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

// SHARD-287: mkdir -p at a mode past the umask, an ls with each entry's own stat, and a delete that takes a full directory only when recursive.
func TestDirectoryVerbsOnTheGuest(t *testing.T) {
	app, id := runningSandbox(t)
	c := app.client()

	if err := c.MakeDir(t.Context(), id, sandbox.MkdirRequest{Path: "/tmp/t287/a/b", Mode: "700", Parents: true}); err != nil {
		t.Fatalf("mkdir -p: %v", err)
	}
	if out, err := runExec(t, app, "exec", id, "--", "/bin/sh", "-c", "stat -c %a /tmp/t287/a/b; touch /tmp/t287/a/b/f; ln -s /tmp/t287/a/b/f /tmp/t287/link"); err != nil || strings.TrimSpace(out) != "700" {
		t.Fatalf("the guest reads %q, %v; want mode 700", out, err)
	}

	entries, err := c.ListDir(t.Context(), id, "/tmp/t287")
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	if len(entries) != 2 || entries[0].Name != "a" || entries[0].Type != models.FileDir || entries[1].Name != "link" || entries[1].Type != models.FileSymlink {
		t.Fatalf("ls = %+v, want the directory a and the symlink link", entries)
	}

	err = c.DeleteFile(t.Context(), id, "/tmp/t287/a", false)
	if err == nil || !strings.Contains(err.Error(), "recursive=true") {
		t.Fatalf("a delete of a full directory gave %v, want a refusal that names recursive=true", err)
	}

	if err := c.DeleteFile(t.Context(), id, "/tmp/t287/link", false); err != nil {
		t.Fatalf("delete the link: %v", err)
	}
	if out, err := runExec(t, app, "exec", id, "--", "/bin/sh", "-c", "test -f /tmp/t287/a/b/f && ! test -L /tmp/t287/link && echo kept"); err != nil || strings.TrimSpace(out) != "kept" {
		t.Fatalf("the guest reads %q, %v; want the link gone and its target kept", out, err)
	}

	if err := c.DeleteFile(t.Context(), id, "/tmp/t287/a", true); err != nil {
		t.Fatalf("delete -r: %v", err)
	}
	if entries, err := c.ListDir(t.Context(), id, "/tmp/t287"); err != nil || len(entries) != 0 {
		t.Fatalf("ls after delete -r = %+v, %v; want nothing", entries, err)
	}

	if err := c.DeleteFile(t.Context(), id, "/", true); err == nil || !strings.Contains(err.Error(), "whole root") {
		t.Fatalf("a delete of / gave %v, want a refusal", err)
	}
}

// SHARD-287: a mkdir as a user runs as that user, so the guest kernel owns the directory and checks the permission.
func TestMakeDirAsAUserOwnsTheDirectory(t *testing.T) {
	app, id := runningSandbox(t)
	c := app.client()

	if err := c.MakeDir(t.Context(), id, sandbox.MkdirRequest{Path: "/tmp/owned", User: "nobody"}); err != nil {
		t.Fatalf("mkdir as nobody: %v", err)
	}
	if out, err := runExec(t, app, "exec", id, "--", "/bin/stat", "-c", "%U %a", "/tmp/owned"); err != nil || strings.TrimSpace(out) != "nobody 755" {
		t.Fatalf("the directory reads %q, %v; want nobody 755", out, err)
	}

	err := c.MakeDir(t.Context(), id, sandbox.MkdirRequest{Path: "/etc/owned", User: "nobody"})
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("a mkdir in /etc as nobody gave %v, want permission denied", err)
	}
}
