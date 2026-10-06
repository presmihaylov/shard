package cli

import (
	"flag"
	"testing"
)

func TestSizeFlagKeepsWholeMiB(t *testing.T) {
	var memory int64
	flags := flag.NewFlagSet("shard create", flag.ContinueOnError)
	flags.Var(sizeMiB{&memory}, "memory", "")

	if err := flags.Parse([]string{"--memory", "2GiB"}); err != nil {
		t.Fatalf("parse --memory 2GiB: %v", err)
	}
	if memory != 2048 || flags.Lookup("memory").Value.String() != "2048" {
		t.Errorf("--memory 2GiB holds %d, want 2048", memory)
	}
}
