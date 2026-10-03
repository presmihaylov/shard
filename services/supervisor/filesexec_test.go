package supervisor

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/presmihaylov/shard/models"
)

// fakeGuest stands in for the provider's exec: it reads the header off stdin and answers as guest says.
func fakeGuest(guest func(header FileHeader, spec models.ExecSpec) models.ExitStatus) ExecFunc {
	return func(_ context.Context, spec models.ExecSpec) (models.ExitStatus, error) {
		var header FileHeader
		if err := ReadHeader(spec.Stdin, &header); err != nil {
			return models.ExitStatus{}, err
		}

		return guest(header, spec), nil
	}
}

func openFake(t *testing.T, ctx context.Context, run ExecFunc) io.ReadWriteCloser {
	t.Helper()
	conn, err := OpenFiles(ctx, run, "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	return conn
}

func TestOpenFilesRunsTheFilesMode(t *testing.T) {
	var got models.ExecSpec
	// The guest reads its request before it answers, so its exit never closes stdin under the host's write (SHARD-443).
	conn, err := OpenFiles(t.Context(), fakeGuest(func(_ FileHeader, spec models.ExecSpec) models.ExitStatus {
		got = spec
		if err := WriteMessage(spec.Stdout, FileReply{Stat: &models.FileStat{Type: models.FileDir}}); err != nil {
			t.Errorf("reply: %v", err)
		}

		return models.ExitStatus{}
	}), "app")
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if _, err := Stat(conn, "/srv"); err != nil {
		t.Fatalf("stat: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if strings.Join(got.Argv, " ") != "/.shard/init files" || got.User != "app" || got.WorkDir != "/" {
		t.Fatalf("the exec ran %+v, want /.shard/init files as app in /", got)
	}
}

// A guest that dies before it answers reads as its exit and its stderr, never as a bare EOF.
func TestAGuestThatDiesReadsAsItsReason(t *testing.T) {
	conn := openFake(t, t.Context(), fakeGuest(func(_ FileHeader, spec models.ExecSpec) models.ExitStatus {
		if _, err := spec.Stderr.WriteString("shard-init: read a files header: boom\n"); err != nil {
			t.Errorf("write stderr: %v", err)
		}

		return models.ExitStatus{Code: 1}
	}))

	_, err := Stat(conn, "/etc/hosts")
	if err == nil || !strings.Contains(err.Error(), "exited 1: shard-init: read a files header: boom") {
		t.Fatalf("stat gave %v, want the exit code and the guest's stderr", err)
	}
	// Every verb joins its read to the close, and the read already said how the exec ended (SHARD-407).
	if joined := errors.Join(err, conn.Close()); strings.Count(joined.Error(), "exited 1") != 1 {
		t.Fatalf("the read joined with the close gave %v, want the exit once", joined)
	}
}

// A failure no read reached, as when the caller stops before the end of a get, is the close's to report.
func TestACloseReportsAFailureNoReadReached(t *testing.T) {
	conn := openFake(t, t.Context(), fakeGuest(func(_ FileHeader, spec models.ExecSpec) models.ExitStatus {
		if err := WriteMessage(spec.Stdout, FileReply{Stat: &models.FileStat{Type: models.FileRegular, Size: 10}}); err != nil {
			t.Errorf("reply: %v", err)
		}
		if _, err := spec.Stderr.WriteString("shard-init: send /srv/blob: input/output error\n"); err != nil {
			t.Errorf("write stderr: %v", err)
		}

		return models.ExitStatus{Code: 1}
	}))

	if _, _, err := Get(conn, "/srv/blob"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := conn.Close(); err == nil || !strings.Contains(err.Error(), "exited 1: shard-init: send /srv/blob: input/output error") {
		t.Fatalf("close gave %v, want the exit and the guest's reason", err)
	}
}

// A /proc file states a size of 0, so a get reads to the end of the stream and never stops at the stat.
func TestAGetReadsPastTheSizeItsStatStates(t *testing.T) {
	conn := openFake(t, t.Context(), fakeGuest(func(_ FileHeader, spec models.ExecSpec) models.ExitStatus {
		if err := WriteMessage(spec.Stdout, FileReply{Stat: &models.FileStat{Type: models.FileRegular, Size: 0}}); err != nil {
			t.Errorf("reply: %v", err)
		}
		if _, err := spec.Stdout.WriteString("Name:\tsh\n"); err != nil {
			t.Errorf("write: %v", err)
		}

		return models.ExitStatus{}
	}))

	_, body, err := Get(conn, "/proc/self/status")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got, err := io.ReadAll(body); err != nil || string(got) != "Name:\tsh\n" {
		t.Fatalf("read %q, %v, want the whole stream", got, err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// A guest that dies midway through a get reads as its reason at the end of the stream, never as a short file.
func TestAGetCutByTheGuestIsAnError(t *testing.T) {
	conn := openFake(t, t.Context(), fakeGuest(func(_ FileHeader, spec models.ExecSpec) models.ExitStatus {
		if err := WriteMessage(spec.Stdout, FileReply{Stat: &models.FileStat{Type: models.FileRegular, Size: 10}}); err != nil {
			t.Errorf("reply: %v", err)
		}
		if _, err := spec.Stdout.WriteString("hello"); err != nil {
			t.Errorf("write: %v", err)
		}
		if _, err := spec.Stderr.WriteString("shard-init: send /srv/blob: input/output error\n"); err != nil {
			t.Errorf("write stderr: %v", err)
		}

		return models.ExitStatus{Code: 1}
	}))
	defer func() { _ = conn.Close() }()

	_, body, err := Get(conn, "/srv/blob")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got, err := io.ReadAll(body); err == nil || !strings.Contains(err.Error(), "input/output error") || string(got) != "hello" {
		t.Fatalf("read %q, %v, want the five bytes and the guest's reason", got, err)
	}
}

// An http body hands over its last bytes with io.EOF, and that is a whole put, not a short one.
func TestAPutWhoseSourceEndsWithItsLastBytesLands(t *testing.T) {
	var landed []byte
	conn := openFake(t, t.Context(), fakeGuest(func(header FileHeader, spec models.ExecSpec) models.ExitStatus {
		body := make([]byte, header.Size)
		if _, err := io.ReadFull(spec.Stdin, body); err != nil {
			t.Errorf("read the put: %v", err)
		}
		landed = body
		if err := WriteMessage(spec.Stdout, FileReply{Stat: &models.FileStat{Type: models.FileRegular, Size: header.Size}}); err != nil {
			t.Errorf("reply: %v", err)
		}

		return models.ExitStatus{}
	}))

	if err := Put(conn, FileHeader{Path: "/srv/app.conf", Size: 5}, iotest.DataErrReader(strings.NewReader("hello"))); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if string(landed) != "hello" {
		t.Fatalf("the guest got %q, want hello", landed)
	}
}

// A refusal keeps the guest's code, so the daemon answers 404 or 400 for it.
func TestARefusalKeepsTheGuestsCode(t *testing.T) {
	conn := openFake(t, t.Context(), fakeGuest(func(header FileHeader, spec models.ExecSpec) models.ExitStatus {
		if err := WriteMessage(spec.Stdout, FileReply{Error: "lstat " + header.Path + ": no such file or directory", Code: FileNotFound}); err != nil {
			t.Errorf("reply: %v", err)
		}

		return models.ExitStatus{}
	}))

	_, err := Stat(conn, "/missing")
	var refusal *FileError
	if !errors.As(err, &refusal) || refusal.Code != FileNotFound || refusal.Op != OpStat || refusal.Path != "/missing" {
		t.Fatalf("stat gave %v, want a not_found FileError for /missing", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// A guest that never answers is freed by the caller's context: the read ends and so does the exec.
func TestTheContextUnblocksAGuestThatHangs(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	conn := openFake(t, ctx, func(_ context.Context, spec models.ExecSpec) (models.ExitStatus, error) {
		var header FileHeader
		if err := ReadHeader(spec.Stdin, &header); err != nil {
			return models.ExitStatus{}, err
		}
		// The caller gives up once the guest holds its request, and the guest waits on stdin, which only the host's close ends.
		cancel()
		if _, err := io.Copy(io.Discard, bufio.NewReader(spec.Stdin)); err != nil {
			return models.ExitStatus{}, err
		}

		return models.ExitStatus{Signal: 9}, nil
	})

	_, err := Stat(conn, "/etc/hosts")
	if err == nil {
		t.Fatal("a stat with no answer succeeded")
	}
	// The read reports the kill when the guest's exit beats the close of its stdout, and the close reports it otherwise.
	if joined := errors.Join(err, conn.Close()); strings.Count(joined.Error(), "signal 9") != 1 {
		t.Fatalf("the stat joined with the close gave %v, want the killed exec once", joined)
	}
}
