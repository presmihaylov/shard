package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
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
	free := func() error { return unreferenced(d, svc, opts.ref) }
	if opts.force {
		free = func() error { return nil }
	}

	if err := a.removeImage(ctx, svc, opts.ref, free); err != nil {
		return err
	}

	return a.print(opts.ref)
}

// imagePrune removes every image no sandbox references, a stopped sandbox being a reference too.
func (a App) imagePrune(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("image prune takes no arguments, got %d", len(args))
	}

	d := a.deps()

	svc, err := d.images()
	if err != nil {
		return err
	}

	images, err := svc.List()
	if err != nil {
		return err
	}

	for _, img := range images {
		// An index entry with no reference is nothing a record could name and nothing Remove could parse.
		if img.Reference == "" {
			a.warn(fmt.Sprintf("image %s has no reference in the index and was left alone", img.Digest))

			continue
		}

		// The check runs under the lock, so a sandbox created since the List keeps its image.
		err := a.removeImage(ctx, svc, img.Reference, func() error { return unreferenced(d, svc, img.Reference) })
		if errors.As(err, &referenced{}) {
			continue
		}
		if err != nil {
			return err
		}

		if err := a.print(img.Reference); err != nil {
			return err
		}
	}

	return nil
}

func (a App) removeImage(ctx context.Context, svc imageService, ref string, free func() error) error {
	err := svc.Remove(ctx, ref, free)
	// The image is gone by this point, so a blob that could not be reclaimed costs disk and not correctness.
	if errors.Is(err, image.ErrNotReclaimed) {
		a.warn(err.Error())

		return nil
	}

	return err
}

// referenced is the refusal a removal gets while a sandbox record still needs the image.
type referenced struct {
	ref   string
	users []string
}

func (e referenced) Error() string {
	return fmt.Sprintf("image %s is referenced by sandbox %s: remove the sandbox first, or pass --force", e.ref, strings.Join(e.users, ", "))
}

// unreferenced refuses when a sandbox record names the image, or one whose rootfs would go with it.
func unreferenced(d *deps, svc imageService, ref string) error {
	held, err := heldImages(d)
	if err != nil {
		return err
	}

	canonical, err := image.Canonical(ref)
	if err != nil {
		return err
	}

	users := held[canonical]

	orphaned, err := svc.Orphaned(ref)
	if err != nil {
		return err
	}

	images, err := svc.List()
	if err != nil {
		return err
	}

	for _, img := range images {
		if img.Reference != canonical && slices.Contains(orphaned, img.Digest) {
			users = append(users, held[img.Reference]...)
		}
	}

	if len(users) != 0 {
		return referenced{ref: ref, users: users}
	}

	return nil
}

// heldImages maps each image reference to the sandboxes whose records name it.
func heldImages(d *deps) (map[string][]string, error) {
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
	// flag stops at the first argument, so a flag after the image would count as a second image.
	if slices.ContainsFunc(rest, func(s string) bool { return strings.HasPrefix(s, "-") }) {
		return imageRemoveOptions{}, fmt.Errorf("image rm takes its flags before the image: shard image rm --force <image>")
	}
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
