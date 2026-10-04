package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/sandbox"
)

// cpTarget is one side of a cp: a host path, or a path in the sandbox ref names.
type cpTarget struct {
	ref  string
	path string
}

// cpOptions is one parsed shard cp invocation; exactly one side names a sandbox.
type cpOptions struct {
	src  cpTarget
	dst  cpTarget
	user string
}

// cp copies a file or a directory between the host and a running sandbox; a refusal is the daemon's own words, which name the op and the path.
func (a App) cp(ctx context.Context, args []string) error {
	opts, err := parseCp(args)
	if err != nil {
		return err
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	if opts.dst.ref != "" {
		return a.cpIn(ctx, c, opts)
	}

	return a.cpOut(ctx, c, opts)
}

func parseCp(args []string) (cpOptions, error) {
	var opts cpOptions

	flags := newFlags("cp")
	flags.StringVar(&opts.user, "user", "", "")

	if err := parseVerb(flags, args); err != nil {
		return cpOptions{}, err
	}
	if flags.NArg() != 2 {
		return cpOptions{}, fmt.Errorf("cp takes a source and a destination, one of them <id|name>:<path>, got %s", gotArgs(flags.Args()))
	}

	opts.src, opts.dst = cpTargetOf(flags.Arg(0)), cpTargetOf(flags.Arg(1))
	if (opts.src.ref == "") == (opts.dst.ref == "") {
		return cpOptions{}, fmt.Errorf("cp copies between the host and a sandbox, so exactly one of %q and %q must be <id|name>:<path>", flags.Arg(0), flags.Arg(1))
	}
	if opts.user != "" && opts.src.ref != "" {
		return cpOptions{}, errors.New("--user is for a copy into a sandbox; a copy out reads as the entrypoint user")
	}

	return opts, nil
}

// cpTargetOf reads id:path as a sandbox side; a host path with a colon in it takes a ./ or a / first, as with docker cp.
func cpTargetOf(arg string) cpTarget {
	ref, guestPath, found := strings.Cut(arg, ":")
	if !found || ref == "" || strings.ContainsRune(ref, '/') {
		return cpTarget{path: arg}
	}

	return cpTarget{ref: ref, path: guestPath}
}

// cpIn puts a host file or directory into the sandbox, under its own name when the destination is a directory.
func (a App) cpIn(ctx context.Context, c *client.Client, opts cpOptions) (err error) {
	// The PathError of open and stat already names the path, so the context added is the copy.
	src, err := os.Open(opts.src.path)
	if err != nil {
		return fmt.Errorf("copy %s to %s:%s: %w", opts.src.path, opts.dst.ref, opts.dst.path, err)
	}
	defer func() { err = errors.Join(err, src.Close()) }()

	info, err := src.Stat()
	if err != nil {
		return fmt.Errorf("copy %s to %s:%s: %w", opts.src.path, opts.dst.ref, opts.dst.path, err)
	}
	if info.IsDir() {
		return a.cpDirIn(ctx, c, opts)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("cp copies a regular file or a directory, and %s is neither", opts.src.path)
	}

	target, err := guestTarget(ctx, c, opts.dst, info.Name())
	if err != nil {
		return err
	}

	req := sandbox.FileWrite{Path: target, Mode: uint32(info.Mode().Perm()), User: opts.user, Size: info.Size()}

	return c.PutFile(ctx, opts.dst.ref, req, src)
}

// guestTarget appends the file's name to a destination that is a directory in the guest.
func guestTarget(ctx context.Context, c *client.Client, dst cpTarget, name string) (string, error) {
	if strings.HasSuffix(dst.path, "/") {
		return path.Join(dst.path, name), nil
	}

	stat, err := c.StatFile(ctx, dst.ref, dst.path)
	// A refused stat has no body to say why, so the put goes ahead and fails, if it does, in the daemon's own words.
	var refused *client.APIError
	if errors.As(err, &refused) {
		return dst.path, nil
	}
	if err != nil {
		return "", err
	}
	if stat.Type == models.FileDir {
		return path.Join(dst.path, name), nil
	}

	return dst.path, nil
}

// cpDirIn streams a host directory to the guest as a tar, which the guest unpacks as the user.
func (a App) cpDirIn(ctx context.Context, c *client.Client, opts cpOptions) error {
	abs, err := filepath.Abs(opts.src.path)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", opts.src.path, err)
	}
	// The walk starts past a link the source path names, as the open of a file follows one.
	tree, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", opts.src.path, err)
	}

	dir, name, err := dirTarget(ctx, c, opts.dst, filepath.Base(abs))
	if err != nil {
		return err
	}
	if name == "/" {
		return fmt.Errorf("a copy of / needs a destination that names its directory, as %s:/srv/root does", opts.dst.ref)
	}

	pr, pw := io.Pipe()
	packed := make(chan error, 1)
	go func() {
		err := client.PackDir(pw, tree, name)
		if err != nil {
			packed <- errors.Join(err, pw.CloseWithError(err))

			return
		}
		packed <- pw.Close()
	}()

	putErr := c.PutArchive(ctx, opts.dst.ref, dir, opts.user, pr)
	// A put that ended early leaves the pack blocked on the pipe, and this close is what frees it.
	if err := pr.CloseWithError(errPutEnded); err != nil {
		return errors.Join(putErr, err)
	}
	if err := <-packed; err != nil && !errors.Is(err, errPutEnded) {
		return fmt.Errorf("pack %s: %w", opts.src.path, err)
	}

	return putErr
}

var errPutEnded = errors.New("the put ended")

// dirTarget answers the guest directory an archive unpacks into and its top entry's name, as docker cp does: into a directory under the source's name, else as dst itself.
func dirTarget(ctx context.Context, c *client.Client, dst cpTarget, name string) (string, string, error) {
	if strings.HasSuffix(dst.path, "/") {
		return dst.path, name, nil
	}

	stat, err := c.StatFile(ctx, dst.ref, dst.path)
	// A refused stat has no body to say why, so the unpack goes ahead and fails, if it does, in the daemon's own words.
	var refused *client.APIError
	if errors.As(err, &refused) {
		return path.Dir(dst.path), path.Base(dst.path), nil
	}
	if err != nil {
		return "", "", err
	}
	if stat.Type == models.FileDir {
		return dst.path, name, nil
	}

	return path.Dir(dst.path), path.Base(dst.path), nil
}

// cpOut writes a guest file to the host through a temp name, so a copy cut midway never leaves a half file at dst; a directory comes as a tar.
func (a App) cpOut(ctx context.Context, c *client.Client, opts cpOptions) (err error) {
	src, err := c.StatFile(ctx, opts.src.ref, opts.src.path)
	// A refused stat has no body to say why, so the get goes ahead and fails in the daemon's own words.
	var refused *client.APIError
	if err != nil && !errors.As(err, &refused) {
		return err
	}
	if err == nil && src.Type == models.FileDir {
		return a.cpDirOut(ctx, c, opts)
	}

	stat, body, err := c.GetFile(ctx, opts.src.ref, opts.src.path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, body.Close()) }()

	target := opts.dst.path
	if info, err := os.Stat(target); err == nil && info.IsDir() {
		target = filepath.Join(target, path.Base(opts.src.path))
	}

	if err := writeAtomic(target, body, os.FileMode(stat.Mode).Perm()); err != nil {
		return fmt.Errorf("copy %s:%s to %s: %w", opts.src.ref, opts.src.path, target, err)
	}

	return nil
}

// cpDirOut unpacks a guest directory's tar at dst, which the sandbox sends and so is untrusted: nothing in it lands outside dst.
func (a App) cpDirOut(ctx context.Context, c *client.Client, opts cpOptions) (err error) {
	name := path.Base(opts.src.path)
	target := opts.dst.path
	if info, err := os.Stat(target); err == nil && info.IsDir() {
		target = filepath.Join(target, name)
	}

	stat, body, err := c.GetArchive(ctx, opts.src.ref, opts.src.path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, body.Close()) }()

	mkdirErr := os.Mkdir(target, 0o700)
	if mkdirErr != nil && !errors.Is(mkdirErr, fs.ErrExist) {
		return fmt.Errorf("make %s: %w", target, mkdirErr)
	}
	made := mkdirErr == nil
	if info, err := os.Lstat(target); err != nil || !info.IsDir() {
		return errors.Join(fmt.Errorf("copy %s:%s to %s, which is not a directory", opts.src.ref, opts.src.path, target), err)
	}

	if err := client.UnpackArchive(body, target, name); err != nil {
		return fmt.Errorf("copy %s:%s to %s: %w", opts.src.ref, opts.src.path, target, err)
	}
	// The unpack leaves a directory already there as it was, so only one this copy made takes the guest's mode.
	if !made {
		return nil
	}
	if err := os.Chmod(target, os.FileMode(stat.Mode).Perm()); err != nil {
		return fmt.Errorf("chmod %s: %w", target, err)
	}

	return nil
}

func writeAtomic(target string, src io.Reader, mode os.FileMode) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".shard-cp-*")
	if err != nil {
		return fmt.Errorf("create a temp file beside %s: %w", target, err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, os.Remove(tmp.Name()))
		}
	}()

	if _, err := io.Copy(tmp, src); err != nil {
		return errors.Join(fmt.Errorf("write %s: %w", tmp.Name(), err), tmp.Close())
	}
	if err := tmp.Chmod(mode); err != nil {
		return errors.Join(fmt.Errorf("chmod %s: %w", tmp.Name(), err), tmp.Close())
	}
	if err := tmp.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync %s: %w", tmp.Name(), err), tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return fmt.Errorf("rename %s over %s: %w", tmp.Name(), target, err)
	}

	return nil
}
