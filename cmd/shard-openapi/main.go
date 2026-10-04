// Command shard-openapi writes the OpenAPI document of the daemon's public routes, which make openapi keeps in docs/openapi.json.
package main

import (
	"fmt"
	"os"

	"github.com/presmihaylov/shard/pkg/store"
	"github.com/presmihaylov/shard/services/api"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: shard-openapi <path>")
		os.Exit(2)
	}

	if err := write(os.Args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "shard-openapi: %v\n", err)
		os.Exit(1)
	}
}

func write(path string) error {
	spec, err := api.Spec()
	if err != nil {
		return err
	}

	if err := store.WriteFile(path, spec, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}
