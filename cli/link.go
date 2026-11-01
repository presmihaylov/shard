package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/presmihaylov/shard/models"
)

func (a App) linkAdd(ctx context.Context, args []string) error {
	rest, err := parseArgs("link add", args)
	if err != nil {
		return err
	}
	if len(rest) != 2 {
		return fmt.Errorf("link add takes a sandbox and the sandbox it links, got %s: shard link add agent1 web1", gotArgs(rest))
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	link, err := c.AddLink(ctx, rest[0], rest[1])
	if err != nil {
		return err
	}

	return a.printLinks([]models.Link{link}, false)
}

func (a App) linkList(ctx context.Context, args []string) error {
	rest, format, err := parseFormatArgs("link list", args, formatTable)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("link list takes one sandbox, got %s: shard link list agent1", gotArgs(rest))
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	links, err := c.ListLinks(ctx, rest[0])
	if err != nil {
		return err
	}
	if format == formatJSON {
		return writeJSON(a.Out, nonNil(links))
	}

	return a.printLinks(links, true)
}

// printLinks puts each link on a line, the target and the address its source reaches it on.
func (a App) printLinks(links []models.Link, header bool) error {
	w := tabwriter.NewWriter(a.Out, 0, 0, 3, ' ', 0)
	if header {
		fmt.Fprintln(w, "TARGET\tADDRESS")
	}
	for _, link := range links {
		fmt.Fprintf(w, "%s\t%s\n", link.Target, link.Address)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("write the output: %w", err)
	}

	return nil
}

func (a App) linkRemove(ctx context.Context, args []string) error {
	rest, err := parseArgs("link remove", args)
	if err != nil {
		return err
	}
	if len(rest) != 2 {
		return fmt.Errorf("link remove takes a sandbox and the sandbox it links, got %s: shard link remove agent1 web1", gotArgs(rest))
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	if err := c.RemoveLink(ctx, rest[0], rest[1]); err != nil {
		return err
	}

	return a.print(rest[1])
}

// linkList is create's --link, repeatable: each sandbox the new one links.
type linkList []string

func (l *linkList) String() string { return strings.Join(*l, ",") }

func (l *linkList) Set(value string) error {
	if value == "" {
		return errors.New("--link names the sandbox to link, got an empty one")
	}
	if slices.Contains(*l, value) {
		return fmt.Errorf("sandbox %s is linked twice; a sandbox links another once", value)
	}

	*l = append(*l, value)

	return nil
}
