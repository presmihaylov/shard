package cli

import (
	"context"
	"fmt"

	"github.com/presmihaylov/shard/services/serve"
)

// serve runs shard serve as its own unprivileged process, so the daemon never binds TCP itself.
func (a App) serve(ctx context.Context, args []string) error {
	flags := newFlags("serve")
	listen := flags.String("listen", serve.DefaultListen, "")
	signingKeyFile := flags.String("signing-key-file", "", "")

	if err := parseVerb(flags, args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("serve takes no arguments, got %s", gotArgs(flags.Args()))
	}
	if err := a.noRemote("serve"); err != nil {
		return err
	}

	return serve.Run(ctx, serve.Config{
		Listen:         *listen,
		SigningKeyFile: *signingKeyFile,
		Root:           a.Root,
		Out:            a.Out,
	})
}
