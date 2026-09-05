package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
)

func (a App) pull(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("pull takes one image reference, got %d", len(args))
	}

	img, err := a.client().PullImage(ctx, args[0])
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
		return a.imageList(ctx, args[1:])
	case "rm", "remove":
		return a.imageRemove(ctx, args[1:])
	case "prune":
		return a.imagePrune(ctx, args[1:])
	}

	return fmt.Errorf("unknown image subcommand %q; want ls, rm or prune", args[0])
}

func (a App) imageList(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("image ls takes no arguments, got %d", len(args))
	}

	images, err := a.client().ListImages(ctx)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(a.Out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "REFERENCE\tDIGEST\tSIZE\tCREATED")

	for _, img := range images {
		size, created := humanSize(img.Size), humanAge(img.Created)
		// An entry the index still names but whose blobs are gone is listed, not hidden and not fatal.
		if img.Broken != "" {
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

	warnings, err := a.client().RemoveImage(ctx, opts.ref, opts.force)
	if err != nil {
		return err
	}
	for _, warning := range warnings {
		a.warn(warning)
	}

	return a.print(opts.ref)
}

// imagePrune removes every image no sandbox references, a stopped sandbox being a reference too.
func (a App) imagePrune(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("image prune takes no arguments, got %d", len(args))
	}

	result, err := a.client().PruneImages(ctx)
	if err != nil {
		return err
	}

	for _, warning := range result.Warnings {
		a.warn(warning)
	}

	for _, ref := range result.Removed {
		if err := a.print(ref); err != nil {
			return err
		}
	}

	return nil
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
