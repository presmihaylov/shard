package cli

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/supervisor"
)

// fakeGuest answers a files exec over root, which stands in for the guest's /.
func fakeGuest(t *testing.T, root string) func(models.ExecSpec) (models.ExitStatus, error) {
	return func(spec models.ExecSpec) (models.ExitStatus, error) {
		var header supervisor.FileHeader
		if err := supervisor.ReadHeader(spec.Stdin, &header); err != nil {
			return models.ExitStatus{}, err
		}
		host := filepath.Join(root, header.Path)

		if header.Op == supervisor.OpPut {
			body := make([]byte, header.Size)
			if _, err := io.ReadFull(spec.Stdin, body); err != nil {
				return models.ExitStatus{}, err
			}
			if err := os.WriteFile(host, body, fs.FileMode(header.Mode)); err != nil {
				return models.ExitStatus{}, err
			}
			if err := os.Chmod(host, fs.FileMode(header.Mode)); err != nil {
				return models.ExitStatus{}, err
			}
		}

		info, err := os.Lstat(host)
		if errors.Is(err, fs.ErrNotExist) {
			return models.ExitStatus{}, supervisor.WriteMessage(spec.Stdout, supervisor.FileReply{Error: "no such file or directory", Code: supervisor.FileNotFound})
		}
		if err != nil {
			return models.ExitStatus{}, err
		}
		stat := models.FileStat{Type: models.FileRegular, Size: info.Size(), Mode: uint32(info.Mode().Perm())}
		if info.IsDir() {
			stat.Type = models.FileDir
		}
		if err := supervisor.WriteMessage(spec.Stdout, supervisor.FileReply{Stat: &stat}); err != nil {
			return models.ExitStatus{}, err
		}

		if header.Op == supervisor.OpGet {
			content, err := os.ReadFile(host)
			if err != nil {
				return models.ExitStatus{}, err
			}
			if _, err := spec.Stdout.Write(content); err != nil {
				return models.ExitStatus{}, err
			}
		}

		return models.ExitStatus{}, nil
	}
}

// newCpApp puts a daemon up over a running sandbox whose files live under the returned guest root.
func newCpApp(t *testing.T) (App, *fakeLifecycleProvider, string) {
	t.Helper()

	var out bytes.Buffer
	app, d := newLifecycleApp(t, &out, &recorder{}, running())
	guestRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(guestRoot, "srv"), 0o755); err != nil {
		t.Fatalf("make the guest /srv: %v", err)
	}
	provider := d.providerSvc.(*fakeLifecycleProvider)
	provider.serve = fakeGuest(t, guestRoot)

	return app, provider, guestRoot
}

func writeHostFile(t *testing.T, name, content string, mode fs.FileMode) string {
	t.Helper()

	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatalf("chmod %s: %v", p, err)
	}

	return p
}

func checkFile(t *testing.T, p, content string, mode fs.FileMode) {
	t.Helper()

	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	if string(got) != content || info.Mode().Perm() != mode {
		t.Fatalf("%s holds %q with mode %v, want %q with %v", p, got, info.Mode().Perm(), content, mode)
	}
}

func TestCpPutsAHostFileWithItsMode(t *testing.T) {
	app, provider, guestRoot := newCpApp(t)
	src := writeHostFile(t, "run.sh", "#!/bin/sh\necho hi\n", 0o750)

	if err := app.Run(t.Context(), []string{"cp", "--user", "app", src, "sandbox1:/srv/start.sh"}); err != nil {
		t.Fatalf("cp: %v", err)
	}

	checkFile(t, filepath.Join(guestRoot, "srv", "start.sh"), "#!/bin/sh\necho hi\n", 0o750)
	if spec := provider.execSpec; spec.User != "app" || strings.Join(spec.Argv, " ") != supervisor.InitPath+" "+supervisor.FilesMode {
		t.Fatalf("the put ran %v as %q, want the files mode as app", spec.Argv, spec.User)
	}
}

func TestCpIntoAGuestDirectoryKeepsTheName(t *testing.T) {
	for _, dst := range []string{"sandbox1:/srv", "sandbox1:/srv/"} {
		t.Run(dst, func(t *testing.T) {
			app, _, guestRoot := newCpApp(t)
			src := writeHostFile(t, "app.conf", "port=80\n", 0o644)

			if err := app.Run(t.Context(), []string{"cp", src, dst}); err != nil {
				t.Fatalf("cp: %v", err)
			}

			checkFile(t, filepath.Join(guestRoot, "srv", "app.conf"), "port=80\n", 0o644)
		})
	}
}

func TestCpCopiesAGuestFileOut(t *testing.T) {
	app, _, guestRoot := newCpApp(t)
	if err := os.WriteFile(filepath.Join(guestRoot, "srv", "report.txt"), []byte("all green\n"), 0o600); err != nil {
		t.Fatalf("seed the guest: %v", err)
	}
	hostDir := t.TempDir()

	if err := app.Run(t.Context(), []string{"cp", "sandbox1:/srv/report.txt", filepath.Join(hostDir, "out.txt")}); err != nil {
		t.Fatalf("cp to a file: %v", err)
	}
	checkFile(t, filepath.Join(hostDir, "out.txt"), "all green\n", 0o600)

	if err := app.Run(t.Context(), []string{"cp", "sandbox1:/srv/report.txt", hostDir}); err != nil {
		t.Fatalf("cp to a directory: %v", err)
	}
	checkFile(t, filepath.Join(hostDir, "report.txt"), "all green\n", 0o600)
}

// A copy out writes a temp name and renames it, so a guest that dies midway leaves the old file and no litter.
func TestCpOutThatIsCutLeavesTheOldFileWhole(t *testing.T) {
	app, provider, _ := newCpApp(t)
	provider.serve = func(spec models.ExecSpec) (models.ExitStatus, error) {
		var header supervisor.FileHeader
		if err := supervisor.ReadHeader(spec.Stdin, &header); err != nil {
			return models.ExitStatus{}, err
		}
		if err := supervisor.WriteMessage(spec.Stdout, supervisor.FileReply{Stat: &models.FileStat{Type: models.FileRegular, Size: 10, Mode: 0o644}}); err != nil {
			return models.ExitStatus{}, err
		}
		if _, err := spec.Stdout.WriteString("hello"); err != nil {
			return models.ExitStatus{}, err
		}

		return models.ExitStatus{Code: 1}, nil
	}
	dst := writeHostFile(t, "blob", "the old file", 0o644)

	if err := app.Run(t.Context(), []string{"cp", "sandbox1:/srv/blob", dst}); err == nil {
		t.Fatal("a copy cut at 5 of 10 bytes succeeded")
	}

	checkFile(t, dst, "the old file", 0o644)
	entries, err := os.ReadDir(filepath.Dir(dst))
	if err != nil || len(entries) != 1 {
		t.Fatalf("the destination directory holds %v, %v; want the old file alone", entries, err)
	}
}

func TestCpKeepsTheGuestsRefusal(t *testing.T) {
	app, _, _ := newCpApp(t)

	err := app.Run(t.Context(), []string{"cp", "sandbox1:/srv/missing", t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "get /srv/missing: no such file or directory") {
		t.Fatalf("cp of a missing file gave %v, want the guest's words", err)
	}
}

func TestCpRefusesADirectorySource(t *testing.T) {
	app, provider, _ := newCpApp(t)

	err := app.Run(t.Context(), []string{"cp", t.TempDir(), "sandbox1:/srv/"})
	if err == nil || !strings.Contains(err.Error(), "is not one") {
		t.Fatalf("cp of a directory gave %v, want a refusal", err)
	}
	if provider.execSpec.Argv != nil {
		t.Fatalf("the refusal still ran an exec: %v", provider.execSpec.Argv)
	}
}

func TestParseCpRefusesWhatItCannotCopy(t *testing.T) {
	for _, args := range [][]string{
		{"/tmp/a"},
		{"/tmp/a", "/tmp/b"},
		{"sb1:/a", "sb2:/b"},
		{"--user", "app", "sb1:/a", "/tmp/b"},
		{"/tmp/a", "sb1:/b", "/tmp/c"},
	} {
		if _, err := parseCp(args); err == nil {
			t.Errorf("parseCp(%v) returned no error", args)
		}
	}
}

func TestCpTargetOfReadsTheSandboxSide(t *testing.T) {
	cases := map[string]cpTarget{
		"sb1:/srv/app":  {ref: "sb1", path: "/srv/app"},
		"web:relative":  {ref: "web", path: "relative"},
		"./a:b":         {path: "./a:b"},
		"/tmp/a:b":      {path: "/tmp/a:b"},
		":/srv":         {path: ":/srv"},
		"plain.txt":     {path: "plain.txt"},
		"dir/file:name": {path: "dir/file:name"},
	}
	for arg, want := range cases {
		if got := cpTargetOf(arg); got != want {
			t.Errorf("cpTargetOf(%q) = %+v, want %+v", arg, got, want)
		}
	}
}
