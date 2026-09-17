package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/presmihaylov/shard/services/serve"
)

// serve runs the TCP front as its own unprivileged process, so the daemon never binds TCP itself.
func (a App) serve(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "mint" {
		return a.serveMint(args[1:])
	}

	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	listen := flags.String("listen", serve.DefaultListen, "the address to accept on")
	cert := flags.String("cert", "", "the tls certificate to serve")
	key := flags.String("key", "", "the key of that certificate")
	secret := flags.String("secret-file", "", "the file holding the secret that signs and checks every token")

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
		Root:       a.Root,
		Out:        a.Out,
	})
}

// serveMint prints one token for a subject, signed by the secret. The daemon never sees the secret.
func (a App) serveMint(args []string) error {
	flags := flag.NewFlagSet("serve mint", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	name := flags.String("name", "", "the subject the token names")
	duration := flags.Duration("duration", 24*time.Hour, "how long the token is valid")
	secretFile := flags.String("secret-file", "", "the file holding the secret that signs the token")
	scopes := flags.String("scopes", "", "a comma-separated list of scopes the token carries; empty is every verb")

	if err := parseVerb(flags, args); err != nil {
		return fmt.Errorf("parse the mint flags: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("mint takes no arguments, got %d", flags.NArg())
	}
	if *name == "" {
		return errors.New("mint needs --name: it is the subject of the token")
	}
	if *duration <= 0 {
		return fmt.Errorf("mint needs a --duration in the future, got %s", *duration)
	}

	secret, err := serve.ReadSecret(*secretFile)
	if err != nil {
		return err
	}

	token, err := serve.Mint(secret, *name, parseScopes(*scopes), *duration)
	if err != nil {
		return err
	}

	return a.print(token)
}

// parseScopes splits a comma-separated --scopes into the list Mint carries; empty is nil, which is every verb.
func parseScopes(raw string) []string {
	var scopes []string
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			scopes = append(scopes, part)
		}
	}

	return scopes
}
