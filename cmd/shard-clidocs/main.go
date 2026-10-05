// Command shard-clidocs writes the CLI reference pages of the site from the help, which make cli-docs keeps current.
package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/presmihaylov/shard/cli"
	"github.com/presmihaylov/shard/pkg/store"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: shard-clidocs <dir>")
		os.Exit(2)
	}

	if err := write(os.Args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "shard-clidocs: %v\n", err)
		os.Exit(1)
	}
}

func write(dir string) error {
	for _, file := range cli.ReferenceFiles() {
		path := filepath.Join(dir, file)
		current, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("read %s: %w", path, err)
		}

		page, err := cli.ReferencePage(file, string(current))
		if err != nil {
			return err
		}
		if err := store.WriteFile(path, []byte(page), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}

	return nil
}
