package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/supervisor"
)

// DefaultFileMode is what a put sets when it names no mode.
const DefaultFileMode = 0o644

// DefaultPutCleanupGrace is how long a put's exec outlives its request, so a guest that got a short stream removes its temp name.
const DefaultPutCleanupGrace = 10 * time.Second

// DefaultDirMode is what a mkdir sets when it names no mode.
const DefaultDirMode = 0o755

// MkdirRequest is the body of POST /mkdir. Mode is octal, as a put's mode= is, so "700" reads the way chmod takes it.
type MkdirRequest struct {
	Path    string `json:"path"`
	Mode    string `json:"mode,omitempty"`
	Parents bool   `json:"parents,omitempty"`
	// User is who the mkdir runs as and who owns the directory, resolved as an exec's user is; empty is the entrypoint's.
	User string `json:"user,omitempty"`
}

// Listing is an ls's entries as they stream: Next answers io.EOF after the last, and Close ends the exec and says whether the guest sent them all.
type Listing interface {
	Next() (models.FileEntry, error)
	Close() error
}

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

// ArchiveWrite is what a put of an archive names: the guest directory it unpacks into, and who that runs as.
type ArchiveWrite struct {
	Path string
	// User is who the unpack runs as and who owns what it lands, resolved as an exec's user is; empty is the entrypoint's.
	User string
}

// ReadArchive answers one guest path's stat and a tar of it as it streams; closing the body says whether the guest sent it all.
func (s *Service) ReadArchive(ctx context.Context, ref, guestPath string) (models.FileStat, io.ReadCloser, error) {
	if err := checkGuestPath(guestPath); err != nil {
		return models.FileStat{}, nil, err
	}

	conn, err := s.openFiles(ctx, ref, "")
	if err != nil {
		return models.FileStat{}, nil, err
	}

	stat, body, err := supervisor.GetArchive(conn, guestPath)
	if err != nil {
		return models.FileStat{}, nil, fileError(errors.Join(err, conn.Close()))
	}

	return stat, &fileBody{Reader: body, conn: conn}, nil
}

// WriteArchive unpacks the tar src under the guest directory req.Path; the guest refuses an entry that would land outside it.
func (s *Service) WriteArchive(ctx context.Context, ref string, req ArchiveWrite, src io.Reader) error {
	if err := checkGuestPath(req.Path); err != nil {
		return err
	}

	conn, err := s.openFiles(ctx, ref, req.User)
	if err != nil {
		return err
	}

	err = supervisor.PutArchive(conn, req.Path, src)

	return fileError(errors.Join(err, conn.Close()))
}

// ListDir streams the entries of one guest directory, sorted by name and each with its own lstat.
func (s *Service) ListDir(ctx context.Context, ref, guestPath string) (Listing, error) {
	if err := checkGuestPath(guestPath); err != nil {
		return nil, err
	}

	conn, err := s.openFiles(ctx, ref, "")
	if err != nil {
		return nil, err
	}

	entries, err := supervisor.List(conn, guestPath)
	if err != nil {
		return nil, fileError(errors.Join(err, conn.Close()))
	}

	return &listing{entries: entries, conn: conn}, nil
}

type listing struct {
	entries *supervisor.Entries
	conn    io.Closer
}

func (l *listing) Next() (models.FileEntry, error) { return l.entries.Next() }

func (l *listing) Close() error { return l.conn.Close() }

// MakeDir makes one guest directory as req.User, at req.Mode past the umask.
func (s *Service) MakeDir(ctx context.Context, ref string, req MkdirRequest) error {
	if err := checkGuestPath(req.Path); err != nil {
		return err
	}
	mode, err := dirModeOf(req.Mode)
	if err != nil {
		return err
	}

	conn, err := s.openFiles(ctx, ref, req.User)
	if err != nil {
		return err
	}

	err = supervisor.Mkdir(conn, supervisor.FileHeader{Path: req.Path, Mode: mode, Parents: req.Parents})

	return fileError(errors.Join(err, conn.Close()))
}

func dirModeOf(raw string) (uint32, error) {
	if raw == "" {
		return DefaultDirMode, nil
	}

	mode, err := strconv.ParseUint(raw, 8, 32)
	if err != nil {
		return 0, &RequestError{Err: fmt.Errorf("a mkdir's mode %q is not an octal mode", raw)}
	}
	if mode > 0o777 {
		return 0, &RequestError{Err: fmt.Errorf("a mkdir's mode is the permission bits, at most 0777, got %#o", mode)}
	}

	return uint32(mode), nil
}

// DeleteFile removes one guest path as the entrypoint user; a directory with anything in it needs recursive, and / is never deleted.
func (s *Service) DeleteFile(ctx context.Context, ref, guestPath string, recursive bool) error {
	if err := checkGuestPath(guestPath); err != nil {
		return err
	}
	if path.Clean(guestPath) == "/" {
		return &RequestError{Err: fmt.Errorf("a delete of %q would take the sandbox's whole root; name what is under it", guestPath)}
	}

	conn, err := s.openFiles(ctx, ref, "")
	if err != nil {
		return err
	}

	err = supervisor.Delete(conn, guestPath, recursive)

	return fileError(errors.Join(err, conn.Close()))
}

// openFiles starts one files exec in a running sandbox, as user; every provider runs the same shard-init mode.
func (s *Service) openFiles(ctx context.Context, ref, user string) (supervisor.FilesConn, error) {
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
	// The exec plumbing around an unknown user says nothing the caller can fix, so only the user goes back.
	var unknown *bundle.UnknownUserError
	if errors.As(err, &unknown) {
		return &RequestError{Err: unknown}
	}

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
