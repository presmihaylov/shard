package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/pkg/sftp"

	"github.com/presmihaylov/shard/services/supervisor"
)

func TestSFTPModeServesTheExecsStdioFromTheUsersHome(t *testing.T) {
	home := t.TempDir()
	cmd := exec.CommandContext(t.Context(), selfBinary, supervisor.SFTPMode)
	cmd.Env = append(os.Environ(), "HOME="+home)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	client, err := sftp.NewClientPipe(stdout, stdin)
	if err != nil {
		t.Fatal(err)
	}
	f, err := client.Create("notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(f, "from scp"); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(f.Close(), client.Close()); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the sftp mode exited %v once its client hung up, want 0", err)
	}

	got, err := os.ReadFile(filepath.Join(home, "notes.txt"))
	if err != nil || string(got) != "from scp" {
		t.Errorf("notes.txt reads %q, %v; want it in HOME", got, err)
	}
}

func TestServeSFTPEndsCleanlyWhenTheClientHangsUp(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	toServer, fromClient := io.Pipe()
	toClient, fromServer := io.Pipe()
	served := make(chan error, 1)
	go func() { served <- serveSFTP(stdio{in: toServer, out: fromServer}, dir) }()

	client, err := sftp.NewClientPipe(toClient, fromClient)
	if err != nil {
		t.Fatal(err)
	}
	f, err := client.Open("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(f)
	if err != nil || string(got) != "abc" {
		t.Fatalf("a.txt reads %q, %v", got, err)
	}
	if err := errors.Join(f.Close(), client.Close()); err != nil {
		t.Fatal(err)
	}
	if err := <-served; err != nil {
		t.Errorf("serveSFTP = %v, want nil at the client's hang up", err)
	}
}
