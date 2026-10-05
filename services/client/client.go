// Package client is the typed side of the daemon's REST API, over the socket or an http or https remote.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/sandbox"
)

// DefaultTimeout bounds one call that answers in full. A call that streams passes zero.
const DefaultTimeout = 30 * time.Second

// DefaultRoot is where shard keeps everything on the box, and the one root a setup service serves.
const DefaultRoot = "/var/lib/shard"

// Client talks to one daemon. It is safe for concurrent use.
type Client struct {
	// target is the socket path or the host url, which is what an error names.
	target string
	// dialer is the whole of the transport switch: the unix socket, tcp to an http remote, or tls to an https one.
	dialer func(ctx context.Context) (net.Conn, error)
	// plain is an http remote, whose bytes, the token among them, cross the network in the clear.
	plain bool
	// authority is the Host of every request: shard on the socket, and otherwise the --remote host a proxy routes by.
	authority string
	// token is the bearer token a front checks. The socket takes none: its mode is the check.
	token string
	// hint is what a connect error tells the operator to check for this target, read only once a dial fails.
	hint func() (string, error)
	http *http.Client
	// Timeout bounds one call. It is not http.Client.Timeout, which would cut a stream; zero is no bound.
	Timeout time.Duration
}

// Version is what the daemon reports for itself, and the API it speaks.
type Version struct {
	Version    string `json:"version"`
	APIVersion string `json:"api_version"`
}

// Sandbox is the public record every sandbox route answers: the host side stays in the daemon's state.
type Sandbox = api.Sandbox

// Inspection is the public record beside the egress rules the host enforces for it.
type Inspection = api.Inspection

// ListResult is what ls prints: the sandboxes the daemon read, beside the records it could not.
type ListResult struct {
	Sandboxes []Sandbox `json:"sandboxes"`
	Warnings  []string  `json:"warnings,omitempty"`
}

// ConnectError is a socket nothing answers on. Its text is the one line the operator needs.
type ConnectError struct {
	Path string
	// Hint is the question and the command that answers it, as is it running? systemctl status shard.
	Hint string
	Err  error
}

func (e *ConnectError) Error() string {
	return fmt.Sprintf("cannot connect to shard daemon at %s: %s", e.Path, e.Hint)
}

func (e *ConnectError) Unwrap() error { return e.Err }

// rootHint is the daemon of root, which someone starts by hand unless setup installed a service for the default root.
func rootHint(root string) string {
	return "is it running? shard --root " + root + " daemon"
}

func fixed(hint string) func() (string, error) {
	return func() (string, error) { return hint, nil }
}

// SetHint replaces the connect hint with one read only once a dial fails, as the CLI reads the host's setup for the default root.
func (c *Client) SetHint(hint func() (string, error)) { c.hint = hint }

// NotFoundError is the daemon's 404: nothing holds the reference.
type NotFoundError struct {
	Ref string
}

func (e *NotFoundError) Error() string { return "no sandbox " + e.Ref }

// APIError is a refusal the daemon answered: its line, its code, the holders an in_use names, and a command_not_started's shell code.
type APIError struct {
	Status   int
	Code     models.Code
	Message  string
	Holders  []string
	ExitCode int
}

func (e *APIError) Error() string { return e.Message }

// New dials the socket under root on the first request.
func New(root string) *Client {
	socket := filepath.Join(root, api.SocketFile)

	c := &Client{target: socket, authority: "shard", hint: fixed(rootHint(root)), Timeout: DefaultTimeout}
	c.dialer = func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}
	c.transport()

	return c
}

// NewRemote dials an http or https remote, a shard serve front or the proxy in front of it, with its bearer token; ca verifies an https remote only.
func NewRemote(host, token string, ca []byte) (*Client, error) {
	parsed, err := parseRemote(host)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, errors.New("--remote needs a token: shard serve answers 401 without one")
	}
	if err := checkToken(token); err != nil {
		return nil, fmt.Errorf("the token %w", err)
	}

	address := remoteAddress(parsed)
	c := &Client{target: host, authority: parsed.Host, token: token, hint: fixed("is it running? shard serve at " + parsed.Host + ", or the proxy in front of it"), Timeout: DefaultTimeout}

	if parsed.Scheme == "http" {
		if len(ca) > 0 {
			return nil, fmt.Errorf("a CA certificate verifies an https remote, and %s is http", host)
		}
		c.plain = true
		c.dialer = func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", address)
		}
		c.transport()

		return c, nil
	}

	settings := &tls.Config{ServerName: parsed.Hostname(), MinVersion: tls.VersionTLS12}
	if len(ca) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return nil, errors.New("the ca file holds no certificate")
		}
		settings.RootCAs = pool
	}
	c.dialer = func(ctx context.Context) (net.Conn, error) {
		return (&tls.Dialer{Config: settings}).DialContext(ctx, "tcp", address)
	}
	c.transport()

	return c, nil
}

// parseRemote takes an http or https url with a host; any other scheme is refused, never guessed.
func parseRemote(host string) (*url.URL, error) {
	parsed, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("parse the host %q: %w", host, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" {
		return nil, fmt.Errorf("--remote must be an http or https url, as https://shard.example.com, got %q", host)
	}

	return parsed, nil
}

// remoteAddress is the host and port a --remote dials: the port of its scheme, 80 or 443, when the url names none.
func remoteAddress(parsed *url.URL) string {
	if parsed.Port() != "" {
		return parsed.Host
	}
	if parsed.Scheme == "http" {
		return net.JoinHostPort(parsed.Hostname(), "80")
	}

	return net.JoinHostPort(parsed.Hostname(), "443")
}

// Plain says the client speaks http to a remote, so nothing encrypts what it sends.
func (c *Client) Plain() bool { return c.plain }

// Format prints the target alone, whatever the verb, so a client in a log line never shows its token.
func (c Client) Format(f fmt.State, _ rune) {
	fmt.Fprintf(f, "shard client for %s", c.target)
}

// endpoint is the url of one route. The dialer picks the connection; the url carries only the path and the Host.
func (c *Client) endpoint(scheme, path string) string {
	return scheme + "://" + c.authority + path
}

// transport sends every request that net/http builds over this client's own dialer.
func (c *Client) transport() {
	c.http = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return c.dial(ctx)
	}}}
}

// dial opens the connection of a plain call. A WebSocket dials through the same dialer, in stream.go.
func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	conn, err := c.dialer(ctx)

	// A certificate the client does not trust is its own error: nothing about the daemon is wrong.
	var untrusted *tls.CertificateVerificationError
	if errors.As(err, &untrusted) {
		return nil, fmt.Errorf("the tls certificate of %s is not trusted: %w", c.target, err)
	}
	if err != nil {
		hint, hintErr := c.hint()
		if hintErr != nil {
			return nil, &ConnectError{Path: c.target, Hint: "cannot tell how this host runs it: " + hintErr.Error(), Err: errors.Join(err, hintErr)}
		}

		return nil, &ConnectError{Path: c.target, Hint: hint, Err: err}
	}

	return conn, nil
}

// Reach opens one connection to the server and closes it, the tls handshake included, so a caller tells a server it cannot reach from one that refuses its token.
func (c *Client) Reach(ctx context.Context) error {
	if c.Timeout != 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	if err := conn.Close(); err != nil {
		return fmt.Errorf("close the connection to %s: %w", c.target, err)
	}

	return nil
}

// authorize carries the bearer token of a front. The socket takes none: its mode is the check.
func (c *Client) authorize(header http.Header) {
	if c.token == "" {
		return
	}

	header.Set("Authorization", "Bearer "+c.token)
}

func (c *Client) Version(ctx context.Context) (Version, error) {
	var out Version
	if err := c.call(ctx, http.MethodGet, "/v0/version", nil, &out, c.Timeout); err != nil {
		return Version{}, err
	}

	return out, nil
}

// Scopes lists every scope a token can carry on the server it speaks to.
func (c *Client) Scopes(ctx context.Context) (api.ScopesResponse, error) {
	var out api.ScopesResponse
	if err := c.call(ctx, http.MethodGet, "/v0/scopes", nil, &out, c.Timeout); err != nil {
		return api.ScopesResponse{}, err
	}

	return out, nil
}

// Capabilities is every lifecycle verb and whether the server supports it.
type Capabilities = api.Capabilities

// Capabilities asks the server which lifecycle verbs it supports; a front answers it for any valid token.
func (c *Client) Capabilities(ctx context.Context) (Capabilities, error) {
	var out Capabilities
	if err := c.call(ctx, http.MethodGet, "/v0/capabilities", nil, &out, c.Timeout); err != nil {
		return Capabilities{}, err
	}

	return out, nil
}

// Daemon is what the daemon reports about its process, its provider and its proxy.
func (c *Client) Daemon(ctx context.Context) (api.Daemon, error) {
	var out api.Daemon
	if err := c.call(ctx, http.MethodGet, "/v0/daemon", nil, &out, c.Timeout); err != nil {
		return api.Daemon{}, err
	}

	return out, nil
}

// ListSandboxes answers the public records, and names the ones the daemon could not read.
func (c *Client) ListSandboxes(ctx context.Context, all bool) (ListResult, error) {
	path := "/v0/sandboxes"
	if all {
		path += "?all=true"
	}

	var out ListResult
	if err := c.call(ctx, http.MethodGet, path, nil, &out, c.Timeout); err != nil {
		return ListResult{}, err
	}

	return out, nil
}

// GetSandbox answers for an id or a name, with the egress rules the host enforces when the record names a policy.
func (c *Client) GetSandbox(ctx context.Context, ref string) (Inspection, error) {
	var out Inspection
	if err := c.call(ctx, http.MethodGet, "/v0/sandboxes/"+url.PathEscape(ref), nil, &out, c.Timeout); err != nil {
		return Inspection{}, missing(ref, err)
	}

	return out, nil
}

// CreateSandbox records the sandbox pending and answers at once; the daemon pulls and starts it in the background.
func (c *Client) CreateSandbox(ctx context.Context, req sandbox.CreateRequest) (Sandbox, error) {
	var out Sandbox
	if err := c.call(ctx, http.MethodPost, "/v0/sandboxes", req, &out, c.Timeout); err != nil {
		return Sandbox{}, err
	}

	return out, nil
}

// WaitSandbox blocks until the sandbox leaves pending, then answers its record. The pull runs in the daemon, so it has no bound of its own.
func (c *Client) WaitSandbox(ctx context.Context, ref string) (Inspection, error) {
	var out Inspection
	if err := c.call(ctx, http.MethodGet, "/v0/sandboxes/"+url.PathEscape(ref)+"?wait=true", nil, &out, 0); err != nil {
		return Inspection{}, missing(ref, err)
	}

	return out, nil
}

func (c *Client) StartSandbox(ctx context.Context, ref string) (Sandbox, error) {
	var out Sandbox
	if err := c.call(ctx, http.MethodPost, "/v0/sandboxes/"+url.PathEscape(ref)+"/start", nil, &out, c.Timeout); err != nil {
		return Sandbox{}, c.missingOrGone(ctx, ref, err)
	}

	return out, nil
}

// StopSandbox waits the grace on top of the usual bound, because the daemon may spend it before it answers.
func (c *Client) StopSandbox(ctx context.Context, ref string) (Sandbox, error) {
	var out Sandbox
	if err := c.call(ctx, http.MethodPost, "/v0/sandboxes/"+url.PathEscape(ref)+"/stop", nil, &out, c.plus(models.StopGrace)); err != nil {
		return Sandbox{}, missing(ref, err)
	}

	return out, nil
}

// GrantSecret hands a created or stopped sandbox the placeholder of a stored secret.
func (c *Client) GrantSecret(ctx context.Context, ref, name string) (Sandbox, error) {
	return c.grant(ctx, http.MethodPost, ref, name)
}

// UngrantSecret takes the placeholder back, and leaves the proxy CA the grant planted.
func (c *Client) UngrantSecret(ctx context.Context, ref, name string) (Sandbox, error) {
	return c.grant(ctx, http.MethodDelete, ref, name)
}

// AttachPolicy gives a created or stopped sandbox the policy the host enforces from its next start.
func (c *Client) AttachPolicy(ctx context.Context, ref, name string) (Sandbox, error) {
	return c.policy(ctx, http.MethodPut, ref, sandbox.PolicyAttachRequest{Policy: name})
}

// DetachPolicy leaves the sandbox with no policy, and with the secrets it holds untouched.
func (c *Client) DetachPolicy(ctx context.Context, ref string) (Sandbox, error) {
	return c.policy(ctx, http.MethodDelete, ref, nil)
}

func (c *Client) policy(ctx context.Context, method, ref string, body any) (Sandbox, error) {
	path := "/v0/sandboxes/" + url.PathEscape(ref) + "/policy"

	var out Sandbox
	if err := c.call(ctx, method, path, body, &out, c.Timeout); err != nil {
		return Sandbox{}, missing(ref, err)
	}

	return out, nil
}

func (c *Client) grant(ctx context.Context, method, ref, name string) (Sandbox, error) {
	path := "/v0/sandboxes/" + url.PathEscape(ref) + "/secrets/" + url.PathEscape(name)

	var out Sandbox
	if err := c.call(ctx, method, path, nil, &out, c.Timeout); err != nil {
		return Sandbox{}, missing(ref, err)
	}

	return out, nil
}

// RemoveSandbox frees a stopped sandbox; force stops a live one first, with the grace a stop gives.
func (c *Client) RemoveSandbox(ctx context.Context, ref string, force bool) error {
	path := "/v0/sandboxes/" + url.PathEscape(ref)
	if force {
		path += "?force=true"
	}

	if err := c.call(ctx, http.MethodDelete, path, nil, nil, c.plus(models.StopGrace)); err != nil {
		return missing(ref, err)
	}

	return nil
}

// PauseSandbox has no bound of its own: a checkpoint takes as long as the memory and the disk it writes.
func (c *Client) PauseSandbox(ctx context.Context, ref string) (Sandbox, error) {
	var out Sandbox
	if err := c.call(ctx, http.MethodPost, "/v0/sandboxes/"+url.PathEscape(ref)+"/pause", nil, &out, 0); err != nil {
		return Sandbox{}, missing(ref, err)
	}

	return out, nil
}

// ResumeSandbox reads back what the pause wrote, so it takes no bound either.
func (c *Client) ResumeSandbox(ctx context.Context, ref string) (Sandbox, error) {
	var out Sandbox
	if err := c.call(ctx, http.MethodPost, "/v0/sandboxes/"+url.PathEscape(ref)+"/resume", nil, &out, 0); err != nil {
		return Sandbox{}, c.missingOrGone(ctx, ref, err)
	}

	return out, nil
}

// ForkSandbox starts a second sandbox from the source's checkpoint.
func (c *Client) ForkSandbox(ctx context.Context, ref string, req sandbox.CopyRequest) (Sandbox, error) {
	var out Sandbox
	if err := c.call(ctx, http.MethodPost, "/v0/sandboxes/"+url.PathEscape(ref)+"/fork", req, &out, 0); err != nil {
		return Sandbox{}, c.missingOrGone(ctx, ref, err)
	}

	return out, nil
}

// plus stretches the bound by what the daemon itself waits for; no bound stays no bound.
func (c *Client) plus(grace time.Duration) time.Duration {
	if c.Timeout == 0 {
		return 0
	}

	return c.Timeout + grace
}

// missing turns the daemon's not_found into the one error a verb prints as its own line.
func missing(ref string, err error) error {
	var answer *APIError
	if errors.As(err, &answer) && answer.Code == models.CodeNotFound {
		return &NotFoundError{Ref: ref}
	}

	return err
}

// missingOrGone tells a sandbox nothing holds from one whose image left the host, since a start, resume or fork answers not_found for both (SHARD-585).
func (c *Client) missingOrGone(ctx context.Context, ref string, err error) error {
	answer, ok := errors.AsType[*APIError](err)
	if !ok || answer.Code != models.CodeNotFound {
		return err
	}
	_, lookErr := c.GetSandbox(ctx, ref)
	if _, nothing := errors.AsType[*NotFoundError](lookErr); nothing {
		return lookErr
	}
	if lookErr != nil {
		return errors.Join(err, fmt.Errorf("look up sandbox %s: %w", ref, lookErr))
	}

	return err
}

// call sends in as JSON and decodes out, each when set, under bound; zero is no deadline.
func (c *Client) call(ctx context.Context, method, path string, in, out any, bound time.Duration) error {
	_, err := c.exchange(ctx, method, path, in, out, bound)

	return err
}

// exchange is call, and also gives back the headers of a successful answer.
func (c *Client) exchange(ctx context.Context, method, path string, in, out any, bound time.Duration) (http.Header, error) {
	call := ctx
	if bound != 0 {
		var cancel context.CancelFunc
		call, cancel = context.WithTimeout(ctx, bound)
		defer cancel()
	}

	var payload io.Reader
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return nil, fmt.Errorf("encode the request for %s %s: %w", method, path, err)
		}
		payload = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(call, method, c.endpoint("http", path), payload)
	if err != nil {
		return nil, fmt.Errorf("build the request for %s %s: %w", method, path, err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.authorize(req.Header)

	resp, err := c.http.Do(req) //nolint:gosec // G704: the ref only lands in the path; the dialer goes to the socket whatever the URL says

	var connect *ConnectError
	if errors.As(err, &connect) {
		return nil, connect
	}
	if err != nil {
		return nil, c.wrap(ctx, method, path, bound, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, c.wrap(ctx, method, path, bound, fmt.Errorf("read the answer: %w", err))
	}

	if resp.StatusCode >= http.StatusBadRequest {
		return nil, decodeError(resp.StatusCode, body)
	}

	if out == nil {
		return resp.Header, nil
	}

	if err := json.Unmarshal(body, out); err != nil {
		return nil, fmt.Errorf("decode the answer to %s %s: %w", method, path, err)
	}

	return resp.Header, nil
}

// wrap names the route and the socket, and says so when the client's own deadline, not the caller's, cut the call.
func (c *Client) wrap(caller context.Context, method, path string, bound time.Duration, err error) error {
	if errors.Is(err, context.DeadlineExceeded) && caller.Err() == nil {
		return fmt.Errorf("%s %s on %s: no answer within %s", method, path, c.target, bound)
	}

	return fmt.Errorf("%s %s on %s: %w", method, path, c.target, unquoted(err))
}

// unquoted drops the url net/http puts on a failed call: it says http:// for a connection the dialer made over tls (SHARD-472).
func unquoted(err error) error {
	var quoted *url.Error
	if errors.As(err, &quoted) {
		return quoted.Err
	}

	return err
}

// decodeError reads the daemon's error object; a body that is not one is quoted as it came, under internal.
func decodeError(status int, body []byte) error {
	var answer struct {
		Error struct {
			Code     models.Code `json:"code"`
			Message  string      `json:"message"`
			Holders  []string    `json:"holders"`
			ExitCode int         `json:"exit_code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &answer); err != nil || answer.Error.Message == "" {
		return &APIError{Status: status, Code: models.CodeInternal, Message: fmt.Sprintf("the daemon answered %d: %q", status, body)}
	}

	return &APIError{Status: status, Code: answer.Error.Code, Message: answer.Error.Message, Holders: answer.Error.Holders, ExitCode: answer.Error.ExitCode}
}

// EgressLog prints one decision per line, oldest first, as the daemon merged the proxy's and the host's, and says on errOut when the daemon left older ones out.
func (c *Client) EgressLog(ctx context.Context, ref string, out, errOut io.Writer) error {
	var records []egress.Record
	header, err := c.exchange(ctx, http.MethodGet, "/v0/sandboxes/"+url.PathEscape(ref)+"/egress-log", nil, &records, c.Timeout)
	if err != nil {
		return missing(ref, err)
	}

	if cut := header.Get(api.EgressCutHeader); cut != "" {
		note := fmt.Sprintf("the egress log of sandbox %s holds %s older decisions; this prints the newest %d\n", ref, cut, len(records))
		if err := write(errOut, []byte(note)); err != nil {
			return fmt.Errorf("write the note on the egress log of sandbox %s: %w", ref, err)
		}
	}

	// One write, so a reader that closes the pipe early (policy logs | grep -q) never leaves the CLI a partial write to SIGPIPE on.
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			return fmt.Errorf("write the egress log of sandbox %s: %w", ref, err)
		}
	}

	if _, err := out.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("write the egress log of sandbox %s: %w", ref, err)
	}

	return nil
}
