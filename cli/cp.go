package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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

// cp copies one file between the host and a running sandbox; a refusal is the daemon's own words, which name the op and the path.
func (a App) cp(ctx context.Context, args []string) error {
	opts, err := parseCp(args)
	if err != nil {
		return err
	}

	if opts.dst.ref != "" {
		return a.cpIn(ctx, opts)
	}

	return a.cpOut(ctx, opts)
}

func parseCp(args []string) (cpOptions, error) {
	var opts cpOptions

	flags := flag.NewFlagSet("shard cp", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.user, "user", "", "the user a copy into the sandbox runs as and who owns the file; empty is the entrypoint's")

	if err := parseVerb(flags, args); err != nil {
		return cpOptions{}, fmt.Errorf("parse the cp flags: %w", err)
	}
	if flags.NArg() != 2 {
		return cpOptions{}, fmt.Errorf("cp takes a source and a destination, one of them <id|name>:<path>, got %d", flags.NArg())
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

// cpIn puts a host file into the sandbox, under its own name when the destination is a directory.
func (a App) cpIn(ctx context.Context, opts cpOptions) (err error) {
	src, err := os.Open(opts.src.path)
	if err != nil {
		return fmt.Errorf("open %s: %w", opts.src.path, err)
	}
	defer func() { err = errors.Join(err, src.Close()) }()

	info, err := src.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", opts.src.path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("cp copies one regular file, and %s is not one", opts.src.path)
	}

	c := a.client()
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

// cpOut writes a guest file to the host through a temp name, so a copy cut midway never leaves a half file at dst.
func (a App) cpOut(ctx context.Context, opts cpOptions) (err error) {
	stat, body, err := a.client().GetFile(ctx, opts.src.ref, opts.src.path)
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
