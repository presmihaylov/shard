package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/presmihaylov/shard/services/client"
)

func (a App) pull(ctx context.Context, args []string) error {
	rest, err := parseArgs("pull", args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("pull takes one image reference, got %s", gotArgs(rest))
	}

	c, err := a.localClient("pull")
	if err != nil {
		return err
	}

	img, err := c.PullImage(ctx, rest[0], a.pullProgress())
	if err != nil {
		return err
	}

	return a.print(fmt.Sprintf("%s\n%s", img.Reference, img.Digest))
}

// pullProgress prints each step of a pull on stderr, so stdout keeps only what a script captures.
func (a App) pullProgress() func(client.PullEvent) {
	if a.Err == nil {
		return nil
	}

	return func(e client.PullEvent) {
		fmt.Fprintln(a.Err, pullLine(e))
	}
}

func pullLine(e client.PullEvent) string {
	switch e.Status {
	case client.PullCached:
		return fmt.Sprintf("%s %s is already on disk%s", e.Reference, e.Digest, at("at", e.Path))
	case client.PullPulling:
		return fmt.Sprintf("pulling %s %s, %s, %s", e.Reference, e.Digest, layerCount(e.Layers), humanSize(e.Bytes))
	case client.PullLayer:
		if e.Present {
			return fmt.Sprintf("  %s  %s, already on disk", shortDigest(e.Digest), humanSize(e.Bytes))
		}

		return fmt.Sprintf("  %s  %s", shortDigest(e.Digest), humanSize(e.Bytes))
	case client.PullUnpacking:
		return fmt.Sprintf("unpacking %s, %s", e.Reference, layerCount(e.Layers))
	case client.PullUnpacked:
		return fmt.Sprintf("  %s  unpacked, %d of %d", shortDigest(e.Digest), e.Layer, e.Layers)
	case client.PullBuilding:
		if e.Path == "" {
			return "  building the image file"
		}

		return "  building " + e.Path
	case client.PullPulled:
		return fmt.Sprintf("pulled %s%s", e.Reference, at("into", e.Path))
	}

	return "pull: " + e.Status
}

// at names the host path an event carries; a create streams none, so its line ends at the image.
func at(preposition, path string) string {
	if path == "" {
		return ""
	}

	return " " + preposition + " " + path
}

func layerCount(n int) string {
	if n == 1 {
		return "1 layer"
	}

	return fmt.Sprintf("%d layers", n)
}

func (a App) imageList(ctx context.Context, args []string) error {
	rest, format, err := parseFormatArgs("image list", args, formatTable)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return fmt.Errorf("image list takes no arguments, got %s", gotArgs(rest))
	}
	c, err := a.localClient("image list")
	if err != nil {
		return err
	}

	images, err := c.ListImages(ctx)
	if err != nil {
		return err
	}
	if format == formatJSON {
		return writeJSON(a.Out, imageViews(images))
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

// imageRemoveOptions is one parsed shard image remove invocation.
type imageRemoveOptions struct {
	ref   string
	force bool
}

func (a App) imageRemove(ctx context.Context, args []string) error {
	opts, err := parseImageRemove(args)
	if err != nil {
		return err
	}

	c, err := a.localClient("image remove")
	if err != nil {
		return err
	}

	warnings, err := c.RemoveImage(ctx, opts.ref, opts.force)
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
	rest, err := parseArgs("image prune", args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return fmt.Errorf("image prune takes no arguments, got %s", gotArgs(rest))
	}

	c, err := a.localClient("image prune")
	if err != nil {
		return err
	}

	result, err := c.PruneImages(ctx)
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

	flags := newFlags("image remove")
	flags.BoolVar(&opts.force, "force", false, "")

	if err := parseVerb(flags, args); err != nil {
		return imageRemoveOptions{}, err
	}

	rest := flags.Args()
	// flag stops at the first argument, so a flag after the image would count as a second image.
	if slices.ContainsFunc(rest, func(s string) bool { return strings.HasPrefix(s, "-") }) {
		return imageRemoveOptions{}, fmt.Errorf("image remove takes its flags before the image: shard image remove --force IMAGE")
	}
	if len(rest) != 1 {
		return imageRemoveOptions{}, fmt.Errorf("image remove takes one image reference, got %s", gotArgs(rest))
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
