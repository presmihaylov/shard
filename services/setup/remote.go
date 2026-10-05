package setup

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/presmihaylov/shard/pkg/term"
	"github.com/presmihaylov/shard/services/client"
)

// verifySteps are the §12 checks, in the order Verify runs them.
var verifySteps = []string{"Reach the server", "Verify authentication", "Read server capabilities"}

// The choices after a failed check, in the order the wizard shows them.
const (
	retryAgain = iota
	retryEdit
	retryExit
)

var retryOptions = []term.Option{
	retryAgain: {Name: "retry", Label: "Retry", Default: true},
	retryEdit:  {Name: "edit", Label: "Edit connection details"},
	retryExit:  {Name: "exit", Label: "Exit"},
}

// remote is §12 and §13: the saved connection's menu when there is one, else a new connection.
func (s *Setup) remote(ctx context.Context) error {
	path, err := client.ConfigPath(s.Host.Env)
	if err != nil {
		return err
	}
	saved, err := client.LoadConfig(path)
	if err != nil {
		return err
	}
	if saved.Remote != "" {
		return s.saved(ctx, path, saved)
	}

	return s.connect(ctx, path, client.Config{})
}

// connect asks for a connection, verifies it, and offers to save it; previous stays on disk until the new one is written.
func (s *Setup) connect(ctx context.Context, path string, previous client.Config) error {
	conn, err := s.ask(ctx, true)
	if err != nil {
		return err
	}
	conn, caps, err := s.verify(ctx, conn)
	if err != nil {
		return err
	}

	return s.offerSave(ctx, path, previous, conn, caps)
}

// verify checks conn until it passes or the user exits, and returns the connection that passed, edited or not.
func (s *Setup) verify(ctx context.Context, conn client.Config) (client.Config, client.Capabilities, error) {
	for {
		list, err := s.UI.Checklist("Checking the connection", verifySteps)
		if err != nil {
			return client.Config{}, client.Capabilities{}, err
		}
		caps, verifyErr := Verify(ctx, s.Host, conn, list)
		if verifyErr == nil {
			return conn, caps, nil
		}

		choice, err := s.UI.Select(ctx, AskRetry, "What would you like to do?", retryOptions)
		if err != nil {
			return client.Config{}, client.Capabilities{}, errors.Join(verifyErr, err)
		}
		if choice == retryExit {
			return client.Config{}, client.Capabilities{}, verifyErr
		}
		if choice == retryEdit {
			if conn, err = s.ask(ctx, false); err != nil {
				return client.Config{}, client.Capabilities{}, err
			}
		}
	}
}

// ask reads the URL and the key; with envKey a key in SHARD_API_KEY is used rather than asked for, and an edit asks the person.
func (s *Setup) ask(ctx context.Context, envKey bool) (client.Config, error) {
	remote, err := s.UI.Text(ctx, AskURL, "Shard server URL:")
	if err != nil {
		return client.Config{}, err
	}
	remote = strings.TrimSpace(remote)

	if parsed, err := url.Parse(remote); err == nil && parsed.Scheme == "http" {
		if err := s.UI.Print("", "HTTP does not encrypt your API key or requests.", "Use it only through a trusted encrypted network.", ""); err != nil {
			return client.Config{}, err
		}
		proceed, err := s.UI.Confirm(ctx, AskHTTP, "Continue?", false)
		if err != nil {
			return client.Config{}, err
		}
		if !proceed {
			return client.Config{}, ErrDeclined
		}
	}

	if key := strings.TrimSpace(s.Host.Env(client.APIKeyEnv)); envKey && key != "" {
		if err := s.UI.Print("Using the API key in " + client.APIKeyEnv + "."); err != nil {
			return client.Config{}, err
		}

		return client.Config{Remote: remote, APIKey: key}, nil
	}

	key, err := s.UI.Secret(ctx, AskAPIKey, "API key:")
	if err != nil {
		return client.Config{}, err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return client.Config{}, fmt.Errorf("setup needs an API key: enter one at the prompt, or set %s", client.APIKeyEnv)
	}

	return client.Config{Remote: remote, APIKey: key}, nil
}

// Verify runs the §12 checks of conn on list and returns the server's capabilities; a failed step shows what to do about it, so its error is a StoppedError.
func Verify(ctx context.Context, host Host, conn client.Config, list Checklist) (client.Capabilities, error) {
	var c *client.Client
	var caps client.Capabilities
	checks := []func() ([]string, error){
		func() ([]string, error) {
			ca, err := client.ReadCA(conn.Remote, host.Env(client.CAFileEnv))
			if err != nil {
				return []string{err.Error()}, err
			}
			if c, err = client.NewRemote(conn.Remote, conn.APIKey, ca); err != nil {
				return []string{err.Error()}, err
			}
			if err := c.Reach(ctx); err != nil {
				return reachDetail(conn.Remote, err), err
			}

			return nil, nil
		},
		func() ([]string, error) {
			if _, err := c.Version(ctx); err != nil {
				return authDetail(err), fmt.Errorf("verify the API key with %s: %w", client.Redacted(conn.Remote), err)
			}

			return nil, nil
		},
		func() (detail []string, err error) {
			if caps, err = c.Capabilities(ctx); err != nil {
				return []string{err.Error()}, fmt.Errorf("read the capabilities of %s: %w", client.Redacted(conn.Remote), err)
			}

			return nil, nil
		},
	}

	for i, check := range checks {
		if err := list.Start(i); err != nil {
			return client.Capabilities{}, err
		}
		if detail, err := check(); err != nil {
			return client.Capabilities{}, errors.Join(&StoppedError{Step: verifySteps[i], Err: err}, list.Fail(i, detail...))
		}
		if err := list.Done(i); err != nil {
			return client.Capabilities{}, err
		}
	}

	return caps, nil
}

// reachDetail says why remote did not answer, and explains SHARD_CA_FILE for a certificate this machine does not trust; there is no way to skip the check.
func reachDetail(remote string, err error) []string {
	var untrusted *tls.CertificateVerificationError
	if errors.As(err, &untrusted) {
		return []string{
			"This machine does not trust the server certificate.",
			"For a private certificate authority, exit, set " + client.CAFileEnv + " to its PEM file, and run shard setup again.",
		}
	}
	var unreachable *client.ConnectError
	if errors.As(err, &unreachable) {
		return []string{
			"Could not reach " + client.Redacted(remote) + ": " + dialCause(unreachable.Err) + ".",
			"Check the URL, and that shard serve or the proxy in front of it runs.",
		}
	}

	return []string{err.Error()}
}

// dialCause words the common dial failures as a person reads them, and leaves any other as the dialer said it.
func dialCause(err error) string {
	var dns *net.DNSError
	var timeout net.Error
	switch {
	case errors.As(err, &dns) && dns.IsNotFound:
		return "no such host"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
		return "connection timed out"
	}

	return err.Error()
}

// authDetail words the 401 of a server that refused the key.
func authDetail(err error) []string {
	var refused *client.APIError
	if errors.As(err, &refused) && refused.Status == http.StatusUnauthorized {
		return []string{"The server did not accept the API key.", "Check the key, or ask the server administrator for a new one."}
	}

	return []string{err.Error()}
}

// offerSave shows the verified connection and saves it on the user's word.
func (s *Setup) offerSave(ctx context.Context, path string, previous, conn client.Config, caps client.Capabilities) error {
	lines := append(connectedLines(conn, caps), "Saving stores your API key as plain text in a file only your user can read.", "")
	if err := s.UI.Print(lines...); err != nil {
		return err
	}

	save, err := s.UI.Confirm(ctx, AskSave, "Save this connection for future Shard commands?", true)
	if err != nil {
		return err
	}
	if !save {
		return s.UI.Print(notSavedLines(previous, conn, s.Host.Env)...)
	}

	if err := client.SaveConfig(path, conn); err != nil {
		return fmt.Errorf("save the connection: %w", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", path, err)
	}

	return s.UI.Print(savedLines(abs, conn, s.Host.Env)...)
}

// connectedLines are the §13 result lines, then every lifecycle capability, the unsupported ones too.
func connectedLines(conn client.Config, caps client.Capabilities) []string {
	lines := []string{
		"✓ Connected to " + client.Redacted(conn.Remote),
		"✓ API key accepted",
		"✓ Server capabilities retrieved",
		"",
		"Server capabilities:",
	}
	for _, capability := range []struct {
		verb      string
		supported bool
	}{
		{"create", caps.Create}, {"start", caps.Start}, {"stop", caps.Stop}, {"remove", caps.Remove},
		{"pause", caps.Pause}, {"resume", caps.Resume}, {"fork", caps.Fork}, {"snapshot", caps.Snapshot},
	} {
		support := "supported"
		if !capability.supported {
			support = "not supported"
		}
		lines = append(lines, fmt.Sprintf("  %-10s %s", capability.verb, support))
	}

	return append(lines, "", "Capabilities show what the server supports. They do not override the permissions of your API key.", "")
}

// savedLines are the §13 completion text, with the variables that still beat what the file says.
func savedLines(path string, conn client.Config, env func(string) string) []string {
	lines := []string{
		"✓ Connection saved",
		"",
		"Configuration: " + path,
		"",
		"The file contains your API key and is accessible only to your user.",
		"Shard will use this connection automatically.",
	}
	if remote := strings.TrimSpace(env(client.RemoteEnv)); remote != "" {
		lines = append(lines, "", client.RemoteEnv+" is set to "+client.Redacted(remote)+", and Shard commands use it before the saved connection.")
	}
	if key := strings.TrimSpace(env(client.APIKeyEnv)); key != "" && key != conn.APIKey {
		lines = append(lines, "", client.APIKeyEnv+" is set to another key, and Shard commands use it before the saved one.")
	}
	if caFile := env(client.CAFileEnv); caFile != "" {
		lines = append(lines, "", "Keep "+client.CAFileEnv+" set: the saved connection does not store the certificate authority.")
	}

	return append(lines,
		"",
		"Next steps:",
		"",
		"  List sandboxes:",
		"    shard list",
		"",
		"  Create a sandbox:",
		"    shard create --name demo --memory 512MiB alpine:3.20",
		"",
		"  Run a command:",
		"    shard exec demo echo hello",
		"",
		"  Remove the sandbox:",
		"    shard remove --force demo",
		"",
		"Documentation: https://useshards.com/docs",
	)
}

// notSavedLines say how to use the verified connection without the file; the key stays a placeholder.
func notSavedLines(previous, conn client.Config, env func(string) string) []string {
	lines := []string{"The connection was verified but not saved."}
	if previous.Remote != "" {
		lines = append(lines, "The saved connection to "+client.Redacted(previous.Remote)+" is unchanged.")
	}
	lines = append(lines,
		"",
		"To use it, set these environment variables:",
		"",
		"  export "+client.RemoteEnv+"="+client.Redacted(conn.Remote),
		"  export "+client.APIKeyEnv+"=<your API key>",
	)
	if caFile := env(client.CAFileEnv); caFile != "" {
		lines = append(lines, "  export "+client.CAFileEnv+"="+caFile)
	}

	return lines
}
