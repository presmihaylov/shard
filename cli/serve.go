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
	cert := flags.String("cert", "", "")
	key := flags.String("key", "", "")
	secret := flags.String("secret-file", "", "")
	tokensFile := flags.String("tokens-file", "", "")

	if err := parseVerb(flags, args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("serve takes no arguments, got %d", flags.NArg())
	}

	return serve.Run(ctx, serve.Config{
		Listen:     *listen,
		CertFile:   *cert,
		KeyFile:    *key,
		SecretFile: *secret,
		TokensFile: *tokensFile,
		Root:       a.Root,
		Out:        a.Out,
	})
}
