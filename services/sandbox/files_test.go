package sandbox_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/supervisor"
)

// guest answers one files exec as shard-init would: it reads the header, then lets answer reply.
func guest(t *testing.T, answer func(header supervisor.FileHeader, spec models.ExecSpec) error) func(models.ExecSpec) (models.ExitStatus, error) {
	return func(spec models.ExecSpec) (models.ExitStatus, error) {
		var header supervisor.FileHeader
		if err := supervisor.ReadHeader(spec.Stdin, &header); err != nil {
			return models.ExitStatus{}, err
		}
		if err := answer(header, spec); err != nil {
			t.Errorf("the fake guest: %v", err)

			return models.ExitStatus{Code: 1}, nil
		}

		return models.ExitStatus{}, nil
	}
}

func refuse(code string) func(supervisor.FileHeader, models.ExecSpec) error {
	return func(header supervisor.FileHeader, spec models.ExecSpec) error {
		return supervisor.WriteMessage(spec.Stdout, supervisor.FileReply{Error: "refused " + header.Path, Code: code})
	}
}

// The daemon reaches the guest through the same path every provider mounts shard-init at.
func TestTheFilesInitPathIsWhereTheBundleMountsShardInit(t *testing.T) {
	if supervisor.InitPath != bundle.GuestInitPath {
		t.Fatalf("supervisor.InitPath is %q, the bundle mounts shard-init at %q", supervisor.InitPath, bundle.GuestInitPath)
	}
}

func TestStatFileRunsTheFilesModeAsTheEntrypointUser(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	want := models.FileStat{Type: models.FileRegular, Size: 12, Mode: 0o4755, UID: 1000, GID: 1000, MTime: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)}
	l.provider.serve = guest(t, func(header supervisor.FileHeader, spec models.ExecSpec) error {
		if header.Op != supervisor.OpStat || header.Path != "/srv/app" {
			t.Errorf("the guest got %+v, want a stat of /srv/app", header)
		}

		return supervisor.WriteMessage(spec.Stdout, supervisor.FileReply{Stat: &want})
	})

	got, err := svc.StatFile(t.Context(), "sandbox1", "/srv/app")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got != want {
		t.Fatalf("stat = %+v, want %+v", got, want)
	}
	spec := l.provider.execSpec
	if strings.Join(spec.Argv, " ") != supervisor.InitPath+" "+supervisor.FilesMode || spec.User != "" || l.provider.execID != "sandbox1" {
		t.Fatalf("the exec ran %v as %q in %s, want the files mode as the entrypoint user in sandbox1", spec.Argv, spec.User, l.provider.execID)
	}
}

func TestWriteFileSendsTheBytesAsTheUser(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	var landed []byte
	l.provider.serve = guest(t, func(header supervisor.FileHeader, spec models.ExecSpec) error {
		if header.Op != supervisor.OpPut || header.Path != "/srv/new/app.conf" || header.Mode != 0o600 || !header.Parents || header.Size != 5 {
			t.Errorf("the guest got %+v, want a put of 5 bytes to /srv/new/app.conf, mode 0600, with parents", header)
		}
		body := make([]byte, header.Size)
		if _, err := io.ReadFull(spec.Stdin, body); err != nil {
			return err
		}
		landed = body

		return supervisor.WriteMessage(spec.Stdout, supervisor.FileReply{Stat: &models.FileStat{Type: models.FileRegular, Size: header.Size}})
	})

	req := sandbox.FileWrite{Path: "/srv/new/app.conf", Mode: 0o600, User: "app", Parents: true, Size: 5}
	if err := svc.WriteFile(t.Context(), "sandbox1", req, strings.NewReader("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if string(landed) != "hello" {
		t.Fatalf("the guest got %q, want hello", landed)
	}
	if l.provider.execSpec.User != "app" {
		t.Fatalf("the put ran as %q, want app", l.provider.execSpec.User)
	}
}

// net/http cancels a request whose body ends short, and the guest still needs its exec to remove the temp name; the grace bounds the wait.
func TestWriteFileKeepsTheExecForTheGuestCleanupAfterTheRequestEnds(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running(), func(c *sandbox.Config) { c.PutCleanupGrace = 50 * time.Millisecond })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var cleanedUp bool
	l.provider.serve = func(spec models.ExecSpec) (models.ExitStatus, error) {
		var header supervisor.FileHeader
		if err := supervisor.ReadHeader(spec.Stdin, &header); err != nil {
			return models.ExitStatus{}, err
		}
		if _, err := io.Copy(io.Discard, spec.Stdin); err != nil {
			return models.ExitStatus{}, err
		}
		// The stream ended short, and an exec still alive here is one the guest can remove its temp name in.
		execCtx := l.provider.execCtx
		cleanedUp = execCtx.Err() == nil
		<-execCtx.Done()

		return models.ExitStatus{Code: 1}, nil
	}

	req := sandbox.FileWrite{Path: "/srv/blob", Mode: 0o600, Size: 10}
	if err := svc.WriteFile(ctx, "sandbox1", req, &cutBody{r: strings.NewReader("hello"), cancel: cancel}); err == nil {
		t.Fatal("a put whose body ended at 5 of 10 bytes succeeded")
	}
	if !cleanedUp {
		t.Fatal("the exec ended with the request, before the guest could remove its temp name")
	}
}

// cutBody hands over its bytes, then fails and cancels the request, as net/http does with a body that ends short.
type cutBody struct {
	r      io.Reader
	cancel context.CancelFunc
}

func (b *cutBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if errors.Is(err, io.EOF) {
		b.cancel()

		return n, io.ErrUnexpectedEOF
	}

	return n, err
}

func TestReadFileStreamsTheWholeFile(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	content := bytes.Repeat([]byte("shard"), 1<<16)
	l.provider.serve = guest(t, func(_ supervisor.FileHeader, spec models.ExecSpec) error {
		if err := supervisor.WriteMessage(spec.Stdout, supervisor.FileReply{Stat: &models.FileStat{Type: models.FileRegular, Size: int64(len(content))}}); err != nil {
			return err
		}
		_, err := spec.Stdout.Write(content)

		return err
	})

	stat, body, err := svc.ReadFile(t.Context(), "sandbox1", "/srv/blob")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read the body: %v", err)
	}
	if err := body.Close(); err != nil {
		t.Fatalf("close the body: %v", err)
	}
	if !bytes.Equal(got, content) || stat.Size != int64(len(content)) {
		t.Fatalf("read %d bytes with a stat of %d, want %d", len(got), stat.Size, len(content))
	}
}

// A guest that dies midway through a get fails the read with its reason, and the close the API joins to it does not say it again (SHARD-407).
func TestReadFileFailsTheCloseOfAGetThatDiedMidway(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	l.provider.serve = func(spec models.ExecSpec) (models.ExitStatus, error) {
		var header supervisor.FileHeader
		if err := supervisor.ReadHeader(spec.Stdin, &header); err != nil {
			return models.ExitStatus{}, err
		}
		if err := supervisor.WriteMessage(spec.Stdout, supervisor.FileReply{Stat: &models.FileStat{Type: models.FileRegular, Size: 10}}); err != nil {
			return models.ExitStatus{}, err
		}
		if _, err := spec.Stdout.WriteString("hello"); err != nil {
			return models.ExitStatus{}, err
		}
		if _, err := spec.Stderr.WriteString("send /srv/blob: input/output error"); err != nil {
			return models.ExitStatus{}, err
		}

		return models.ExitStatus{Code: 1}, nil
	}

	_, body, err := svc.ReadFile(t.Context(), "sandbox1", "/srv/blob")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	_, err = io.ReadAll(body)
	if err == nil {
		t.Fatal("a body cut at 5 of 10 bytes read to its end with no error")
	}
	if joined := errors.Join(err, body.Close()); strings.Count(joined.Error(), "input/output error") != 1 {
		t.Fatalf("the read joined with the close gave %v, want the guest's reason once", joined)
	}
}

func TestAFileRefusalAnswersTheAPICode(t *testing.T) {
	cases := []struct {
		code     string
		notFound bool
		request  bool
	}{
		{code: supervisor.FileNotFound, notFound: true},
		{code: supervisor.FileInvalid, request: true},
		{code: ""},
	}
	for _, c := range cases {
		t.Run("code "+c.code, func(t *testing.T) {
			r := &recorder{}
			svc, l := newService(t, r, running())
			l.provider.serve = guest(t, refuse(c.code))

			_, err := svc.StatFile(t.Context(), "sandbox1", "/srv/app")
			var notFound *sandbox.FileNotFoundError
			var request *sandbox.RequestError
			if err == nil || errors.As(err, &notFound) != c.notFound || errors.As(err, &request) != c.request {
				t.Fatalf("stat gave %v, want not found %v and a request error %v", err, c.notFound, c.request)
			}
			if !strings.Contains(err.Error(), "refused /srv/app") {
				t.Fatalf("stat gave %v, want the guest's words", err)
			}
		})
	}
}

func TestListDirStreamsTheEntriesAsTheEntrypointUser(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	want := []models.FileEntry{
		{Name: "app", FileStat: models.FileStat{Type: models.FileRegular, Size: 3, Mode: 0o755}},
		{Name: "conf", FileStat: models.FileStat{Type: models.FileDir, Mode: 0o700}},
	}
	l.provider.serve = guest(t, func(header supervisor.FileHeader, spec models.ExecSpec) error {
		if header.Op != supervisor.OpList || header.Path != "/srv" {
			t.Errorf("the guest got %+v, want an ls of /srv", header)
		}
		if err := supervisor.WriteMessage(spec.Stdout, supervisor.FileReply{Stat: &models.FileStat{Type: models.FileDir}, Count: len(want)}); err != nil {
			return err
		}
		for _, entry := range want {
			if err := supervisor.WriteMessage(spec.Stdout, entry); err != nil {
				return err
			}
		}

		return nil
	})

	listing, err := svc.ListDir(t.Context(), "sandbox1", "/srv")
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	var got []models.FileEntry
	for {
		entry, err := listing.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		got = append(got, entry)
	}
	if err := listing.Close(); err != nil {
		t.Fatalf("close the listing: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ls = %+v, want %+v", got, want)
	}
	if l.provider.execSpec.User != "" {
		t.Fatalf("the ls ran as %q, want the entrypoint user", l.provider.execSpec.User)
	}
}

func TestMakeDirSendsTheModeAndParentsAsTheUser(t *testing.T) {
	cases := []struct {
		mode string
		want uint32
	}{
		{mode: "", want: sandbox.DefaultDirMode},
		{mode: "700", want: 0o700},
		{mode: "0750", want: 0o750},
	}
	for _, c := range cases {
		t.Run("mode "+c.mode, func(t *testing.T) {
			r := &recorder{}
			svc, l := newService(t, r, running())
			l.provider.serve = guest(t, func(header supervisor.FileHeader, spec models.ExecSpec) error {
				if header != (supervisor.FileHeader{Op: supervisor.OpMkdir, Path: "/srv/a/b", Mode: c.want, Parents: true}) {
					t.Errorf("the guest got %+v, want a mkdir -p of /srv/a/b at %#o", header, c.want)
				}

				return supervisor.WriteMessage(spec.Stdout, supervisor.FileReply{Stat: &models.FileStat{Type: models.FileDir, Mode: header.Mode}})
			})

			if err := svc.MakeDir(t.Context(), "sandbox1", sandbox.MkdirRequest{Path: "/srv/a/b", Mode: c.mode, Parents: true, User: "app"}); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if l.provider.execSpec.User != "app" {
				t.Fatalf("the mkdir ran as %q, want app", l.provider.execSpec.User)
			}
		})
	}
}

func TestDeleteFileSendsRecursiveAsTheEntrypointUser(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	l.provider.serve = guest(t, func(header supervisor.FileHeader, spec models.ExecSpec) error {
		if header != (supervisor.FileHeader{Op: supervisor.OpDelete, Path: "/srv/cache", Recursive: true}) {
			t.Errorf("the guest got %+v, want a recursive delete of /srv/cache", header)
		}

		return supervisor.WriteMessage(spec.Stdout, supervisor.FileReply{Stat: &models.FileStat{Type: models.FileDir}})
	})

	if err := svc.DeleteFile(t.Context(), "sandbox1", "/srv/cache", true); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if l.provider.execSpec.User != "" {
		t.Fatalf("the delete ran as %q, want the entrypoint user", l.provider.execSpec.User)
	}
}

// What the daemon can refuse on its own costs no exec.
func TestFileVerbsRefuseWithNoExec(t *testing.T) {
	cases := []struct {
		name string
		sb   models.Sandbox
		run  func(svc *sandbox.Service) error
		want func(error) bool
	}{
		{
			name: "a relative path",
			sb:   running(),
			run: func(svc *sandbox.Service) error {
				_, err := svc.StatFile(t.Context(), "sandbox1", "srv/app")

				return err
			},
			want: isRequestError,
		},
		{
			name: "a mode above 0777",
			sb:   running(),
			run: func(svc *sandbox.Service) error {
				return svc.WriteFile(t.Context(), "sandbox1", sandbox.FileWrite{Path: "/srv/app", Mode: 0o4755}, strings.NewReader(""))
			},
			want: isRequestError,
		},
		{
			name: "a mkdir mode that is not octal",
			sb:   running(),
			run: func(svc *sandbox.Service) error {
				return svc.MakeDir(t.Context(), "sandbox1", sandbox.MkdirRequest{Path: "/srv/a", Mode: "rwx"})
			},
			want: isRequestError,
		},
		{
			name: "a mkdir mode above 0777",
			sb:   running(),
			run: func(svc *sandbox.Service) error {
				return svc.MakeDir(t.Context(), "sandbox1", sandbox.MkdirRequest{Path: "/srv/a", Mode: "1777"})
			},
			want: isRequestError,
		},
		{
			name: "a relative ls",
			sb:   running(),
			run: func(svc *sandbox.Service) error {
				_, err := svc.ListDir(t.Context(), "sandbox1", "srv")

				return err
			},
			want: isRequestError,
		},
		{
			name: "a delete of the root",
			sb:   running(),
			run: func(svc *sandbox.Service) error {
				return svc.DeleteFile(t.Context(), "sandbox1", "/", true)
			},
			want: isRequestError,
		},
		{
			name: "a delete of the root spelled another way",
			sb:   running(),
			run: func(svc *sandbox.Service) error {
				return svc.DeleteFile(t.Context(), "sandbox1", "//srv/..", true)
			},
			want: isRequestError,
		},
		{
			name: "a delete in a stopped sandbox",
			sb:   stopped(),
			run: func(svc *sandbox.Service) error {
				return svc.DeleteFile(t.Context(), "sandbox1", "/srv/a", false)
			},
			want: isNotRunning,
		},
		{
			name: "a stopped sandbox",
			sb:   stopped(),
			run: func(svc *sandbox.Service) error {
				_, _, err := svc.ReadFile(t.Context(), "sandbox1", "/srv/app")

				return err
			},
			want: isNotRunning,
		},
		{
			name: "a paused sandbox",
			sb:   pausedSandbox(),
			run: func(svc *sandbox.Service) error {
				return svc.WriteFile(t.Context(), "sandbox1", sandbox.FileWrite{Path: "/srv/app", Mode: sandbox.DefaultFileMode}, strings.NewReader(""))
			},
			want: isNotRunning,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &recorder{}
			svc, _ := newService(t, r, c.sb)

			if err := c.run(svc); !c.want(err) {
				t.Fatalf("got %v", err)
			}
			if calls := keep(r.calls, "provider.Exec"); len(calls) != 0 {
				t.Fatalf("the refusal ran an exec: %v", calls)
			}
		})
	}
}

func isRequestError(err error) bool {
	var request *sandbox.RequestError

	return errors.As(err, &request)
}

func isNotRunning(err error) bool {
	var state *sandbox.StateError

	return errors.As(err, &state) && state.Code == models.CodeSandboxNotRunning
}
