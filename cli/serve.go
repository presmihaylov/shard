package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/presmihaylov/shard/services/serve"
)

// serve runs the TCP front of the daemon: TLS, a bearer token, and then the socket, byte for byte.
// It is a process of its own, unprivileged, so the daemon never binds TCP itself.
func (a App) serve(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	listen := flags.String("listen", serve.DefaultListen, "the address to accept on")
	cert := flags.String("cert", "", "the tls certificate to serve")
	key := flags.String("key", "", "the key of that certificate")
	token := flags.String("token-file", "", "the file holding the bearer token every request carries")

	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse the serve flags: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("serve takes no arguments, got %d", flags.NArg())
	}

	return serve.Run(ctx, serve.Config{
		Listen:    *listen,
		CertFile:  *cert,
		KeyFile:   *key,
		TokenFile: *token,
		Root:      a.Root,
		Out:       a.Out,
	})
}
