package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/presmihaylov/shard/services/serve"
)

// serve runs the TCP front as its own unprivileged process, so the daemon never binds TCP itself.
func (a App) serve(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	listen := flags.String("listen", serve.DefaultListen, "the address to accept on")
	cert := flags.String("cert", "", "the tls certificate to serve")
	key := flags.String("key", "", "the key of that certificate")
	secret := flags.String("secret-file", "", "the file holding the secret that signs and checks every token")
	tokensFile := flags.String("tokens-file", "", "the ledger of minted tokens; overrides the one beside the secret file")

	if err := parseVerb(flags, args); err != nil {
		return fmt.Errorf("parse the serve flags: %w", err)
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
