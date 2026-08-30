package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/presmihaylov/shard/services/image"
)

func (a App) pull(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("pull takes one image reference, got %d", len(args))
	}

	svc, err := a.deps().images()
	if err != nil {
		return err
	}

	// A registry that accepts the connection and then stalls would otherwise pin this process forever.
	ctx, cancel := context.WithTimeout(ctx, a.Timeout)
	defer cancel()

	img, err := svc.Pull(ctx, args[0])
	if err != nil {
		return err
	}

	return a.print(fmt.Sprintf("%s\n%s", img.Reference, img.Digest))
}

func (a App) image(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("image takes a subcommand: ls, rm or prune")
	}

	switch args[0] {
	case "ls", "list":
		return a.imageList(args[1:])
	case "rm", "remove":
		return a.imageRemove(ctx, args[1:])
	case "prune":
		return a.imagePrune(ctx, args[1:])
	}

	return fmt.Errorf("unknown image subcommand %q; want ls, rm or prune", args[0])
}

func (a App) imageList(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("image ls takes no arguments, got %d", len(args))
	}

	svc, err := a.deps().images()
	if err != nil {
		return err
	}

	images, err := svc.List()
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(a.Out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "REFERENCE\tDIGEST\tSIZE\tCREATED")

	for _, img := range images {
		size, created := humanSize(img.Size), humanAge(img.Created)
		// An entry the index still names but whose blobs are gone is listed, not hidden and not fatal.
		if img.Broken != nil {
			size, created = "unreadable", "unreadable"
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", img.Reference, shortDigest(img.Digest), size, created)
	}

	if err := w.Flush(); err != nil {
		return fmt.Errorf("write the output: %w", err)
	}

	return nil
}

// imageRemoveOptions is one parsed shard image rm invocation.
type imageRemoveOptions struct {
	ref   string
	force bool
}

func (a App) imageRemove(ctx context.Context, args []string) error {
	opts, err := parseImageRemove(args)
	if err != nil {
		return err
	}

	d := a.deps()

	svc, err := d.images()
	if err != nil {
		return err
	}

	// A sandbox runs on the rootfs the image unpacked to, so removing it under one pulls the floor away.
	if !opts.force {
		if err := a.unreferenced(d, opts.ref); err != nil {
			return err
		}
	}

	if err := a.removeImage(ctx, svc, opts.ref); err != nil {
		return err
	}

	return a.print(opts.ref)
}

// imagePrune removes every image no sandbox references, stopped ones included: a stopped sandbox
// starts again on the same rootfs.
func (a App) imagePrune(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("image prune takes no arguments, got %d", len(args))
	}

	d := a.deps()

	svc, err := d.images()
	if err != nil {
		return err
	}

	held, err := a.heldImages(d)
	if err != nil {
		return err
	}

	images, err := svc.List()
	if err != nil {
		return err
	}

	for _, img := range images {
		if len(held[img.Reference]) != 0 {
			continue
		}

		if err := a.removeImage(ctx, svc, img.Reference); err != nil {
			return err
		}

		if err := a.print(img.Reference); err != nil {
			return err
		}
	}

	return nil
}

func (a App) removeImage(ctx context.Context, svc imageService, ref string) error {
	err := svc.Remove(ctx, ref)
	// The image is gone by this point, so a blob that could not be reclaimed costs disk and not correctness.
	if errors.Is(err, image.ErrNotReclaimed) {
		a.warn(err.Error())

		return nil
	}

	return err
}

// unreferenced refuses when a sandbox record names the image.
func (a App) unreferenced(d *deps, ref string) error {
	held, err := a.heldImages(d)
	if err != nil {
		return err
	}

	canonical, err := image.Canonical(ref)
	if err != nil {
		return err
	}

	if users := held[canonical]; len(users) != 0 {
		return fmt.Errorf("image %s is referenced by sandbox %s: remove the sandbox first, or pass --force", ref, strings.Join(users, ", "))
	}

	return nil
}

// heldImages maps each image reference to the sandboxes whose records name it.
func (a App) heldImages(d *deps) (map[string][]string, error) {
	repo, err := d.repo()
	if err != nil {
		return nil, err
	}

	sandboxes, unreadable := repo.List()
	// A record that does not read back may name the image, so nothing can say it is free.
	if unreadable != nil {
		return nil, fmt.Errorf("cannot tell which images the sandboxes reference: %w", unreadable)
	}

	held := map[string][]string{}
	for _, sb := range sandboxes {
		held[sb.Image] = append(held[sb.Image], sb.ID)
	}

	return held, nil
}

func parseImageRemove(args []string) (imageRemoveOptions, error) {
	var opts imageRemoveOptions

	flags := flag.NewFlagSet("shard image rm", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.BoolVar(&opts.force, "force", false, "remove the image even when a sandbox references it")

	if err := flags.Parse(args); err != nil {
		return imageRemoveOptions{}, fmt.Errorf("parse the image rm flags: %w", err)
	}

	rest := flags.Args()
	if len(rest) != 1 {
		return imageRemoveOptions{}, fmt.Errorf("image rm takes one image reference, got %d", len(rest))
	}

	opts.ref = rest[0]

	return opts, nil
}

func shortDigest(digest string) string {
	hex := digest
	if _, after, found := strings.Cut(digest, ":"); found {
		hex = after
	}

	if len(hex) <= 12 {
		return hex
	}

	return hex[:12]
}

func humanSize(bytes int64) string {
	const unit = 1000

	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}

	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "kMGT"[exp])
}

func humanAge(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}

	return t.Format(time.RFC3339)
}
