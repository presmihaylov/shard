package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/presmihaylov/shard/services/image"
)

func (a App) pull(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("pull takes one image reference, got %d", len(args))
	}

	svc, err := a.images()
	if err != nil {
		return err
	}

	img, err := svc.Pull(ctx, args[0])
	if err != nil {
		return err
	}

	return a.print(fmt.Sprintf("%s\n%s", img.Reference, img.Digest))
}

func (a App) image(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("image takes a subcommand: ls or rm")
	}

	switch args[0] {
	case "ls", "list":
		return a.imageList(args[1:])
	case "rm", "remove":
		return a.imageRemove(args[1:])
	}

	return fmt.Errorf("unknown image subcommand %q; want ls or rm", args[0])
}

func (a App) imageList(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("image ls takes no arguments, got %d", len(args))
	}

	svc, err := a.images()
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
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", img.Reference, shortDigest(img.Digest), humanSize(img.Size), humanAge(img.Created))
	}

	if err := w.Flush(); err != nil {
		return fmt.Errorf("write the output: %w", err)
	}

	return nil
}

func (a App) imageRemove(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("image rm takes one image reference, got %d", len(args))
	}

	svc, err := a.images()
	if err != nil {
		return err
	}

	if err := svc.Remove(args[0]); err != nil {
		return err
	}

	return a.print(args[0])
}

func (a App) images() (*image.Service, error) {
	return image.New(filepath.Join(a.Root, "images"))
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
