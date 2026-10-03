package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/supervisor"
)

// DefaultFileMode is what a put sets when it names no mode.
const DefaultFileMode = 0o644

// DefaultPutCleanupGrace is how long a put's exec outlives its request, so a guest that got a short stream removes its temp name.
const DefaultPutCleanupGrace = 10 * time.Second

// FileWrite is what a put names: where the file lands, its mode, who owns it, and how many bytes follow.
type FileWrite struct {
	Path string
	Mode uint32
	// User is who the put runs as and who owns the file, resolved as an exec's user is; empty is the entrypoint's.
	User    string
	Parents bool
	Size    int64
}

// FileNotFoundError is a guest path that does not exist, which the API answers 404 for.
type FileNotFoundError struct {
	Err error
}

func (e *FileNotFoundError) Error() string { return e.Err.Error() }

func (e *FileNotFoundError) Unwrap() error { return e.Err }

// StatFile answers the shape of one guest path, never following a final symlink.
func (s *Service) StatFile(ctx context.Context, ref, guestPath string) (models.FileStat, error) {
	if err := checkGuestPath(guestPath); err != nil {
		return models.FileStat{}, err
	}

	conn, err := s.openFiles(ctx, ref, "")
	if err != nil {
		return models.FileStat{}, err
	}

	stat, err := supervisor.Stat(conn, guestPath)

	return stat, fileError(errors.Join(err, conn.Close()))
}

// ReadFile answers one guest file's stat and its bytes as they stream; closing the body ends the exec.
func (s *Service) ReadFile(ctx context.Context, ref, guestPath string) (models.FileStat, io.ReadCloser, error) {
	if err := checkGuestPath(guestPath); err != nil {
		return models.FileStat{}, nil, err
	}

	conn, err := s.openFiles(ctx, ref, "")
	if err != nil {
		return models.FileStat{}, nil, err
	}

	stat, body, err := supervisor.Get(conn, guestPath)
	if err != nil {
		return models.FileStat{}, nil, fileError(errors.Join(err, conn.Close()))
	}

	return stat, &fileBody{Reader: body, conn: conn}, nil
}

// fileBody is a get's bytes; its Close says whether the guest sent them all, so a short file is never a success.
type fileBody struct {
	io.Reader
	conn io.Closer
}

func (b *fileBody) Close() error {
	return b.conn.Close()
}

// WriteFile lands Size bytes of src at the guest path as one file: the old file stays whole until the new one is in place.
func (s *Service) WriteFile(ctx context.Context, ref string, req FileWrite, src io.Reader) error {
	if err := checkGuestPath(req.Path); err != nil {
		return err
	}
	if req.Mode > 0o777 {
		return &RequestError{Err: fmt.Errorf("a put's mode is the permission bits, at most 0777, got %#o", req.Mode)}
	}
	if req.Size < 0 {
		return &RequestError{Err: fmt.Errorf("a put needs the size of its body, got %d", req.Size)}
	}

	// net/http cancels a request whose body ends short, and an exec cancelled with it dies before the guest removes its temp name.
	execCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, func() { time.AfterFunc(s.putCleanupGrace(), cancel) })
	defer func() {
		stop()
		cancel()
	}()

	conn, err := s.openFiles(execCtx, ref, req.User)
	if err != nil {
		return err
	}

	err = supervisor.Put(conn, supervisor.FileHeader{Path: req.Path, Size: req.Size, Mode: req.Mode, Parents: req.Parents}, src)

	return fileError(errors.Join(err, conn.Close()))
}

func (s *Service) putCleanupGrace() time.Duration {
	if s.cfg.PutCleanupGrace != 0 {
		return s.cfg.PutCleanupGrace
	}

	return DefaultPutCleanupGrace
}

// openFiles starts one files exec in a running sandbox, as user; every provider runs the same shard-init mode.
func (s *Service) openFiles(ctx context.Context, ref, user string) (io.ReadWriteCloser, error) {
	id, err := s.readyForExec(ctx, ref)
	if err != nil {
		return nil, err
	}

	conn, err := supervisor.OpenFiles(ctx, func(ctx context.Context, spec models.ExecSpec) (models.ExitStatus, error) {
		return s.cfg.Provider.Exec(ctx, id, spec)
	}, user)
	if err != nil {
		return nil, fmt.Errorf("sandbox %s: %w", id, err)
	}

	return conn, nil
}

// checkGuestPath refuses a relative path before any exec: shard has no default directory to resolve it in.
func checkGuestPath(guestPath string) error {
	if !path.IsAbs(guestPath) {
		return &RequestError{Err: fmt.Errorf("a guest path must be absolute, got %q", guestPath)}
	}

	return nil
}

// fileError maps the guest's refusal to the error the API answers: not_found is 404, invalid is 400, the rest is 500.
func fileError(err error) error {
	var refusal *supervisor.FileError
	if !errors.As(err, &refusal) {
		return err
	}

	switch refusal.Code {
	case supervisor.FileNotFound:
		return &FileNotFoundError{Err: err}
	case supervisor.FileInvalid:
		return &RequestError{Err: err}
	}

	return err
}
