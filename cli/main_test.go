//go:build !integration

package cli

import (
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	cleanup, err := isolateConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cli tests: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()
	if err := cleanup(); err != nil {
		fmt.Fprintf(os.Stderr, "cli tests: %v\n", err)
		code = 1
	}

	os.Exit(code)
}
