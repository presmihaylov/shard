package client

import (
	"cmp"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// The environment a remote client reads, as docker's reads DOCKER_HOST. The CLI and NewRemoteFromEnv share it.
const (
	RemoteEnv = "SHARD_REMOTE"
	APIKeyEnv = "SHARD_API_KEY" //nolint:gosec // G101: this names the variable, and holds no key
	CAFileEnv = "SHARD_CA_FILE"
)

// NewRemoteFromEnv builds the client of host, or of SHARD_REMOTE, or of the saved remote, with SHARD_API_KEY or the saved key, and SHARD_CA_FILE.
func NewRemoteFromEnv(host string, saved Config) (*Client, error) {
	host = cmp.Or(host, os.Getenv(RemoteEnv), saved.Remote)
	if host == "" {
		return nil, fmt.Errorf("no server to connect to: pass --remote, set %s, or run shard setup, as in --remote https://shard.example.com", RemoteEnv)
	}
	parsed, err := parseRemote(host)
	if err != nil {
		return nil, err
	}

	token, refused, err := apiKey(parsed, saved)
	if err != nil {
		return nil, err
	}

	ca, err := ReadCA(host, os.Getenv(CAFileEnv))
	if err != nil {
		return nil, err
	}

	c, err := NewRemote(host, token, ca)
	if err != nil {
		return nil, err
	}
	c.refused = refused

	return c, nil
}

// ReadCA reads the certificate caFile, SHARD_CA_FILE, holds for host, or none when it is empty.
func ReadCA(host, caFile string) ([]byte, error) {
	if caFile == "" {
		return nil, nil
	}
	parsed, err := parseRemote(host)
	if err != nil {
		return nil, err
	}
	// Refused before the file is read, so the mismatch is the one error, whatever the file holds.
	if parsed.Scheme == "http" {
		return nil, fmt.Errorf("%s applies only to an https remote, and %s is http; use https, or unset %s", CAFileEnv, host, CAFileEnv)
	}
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read the CA certificate file %s from %s: %w", caFile, CAFileEnv, err)
	}

	return ca, nil
}

// apiKey is SHARD_API_KEY, else the saved key when remote is the saved remote, so a key reaches no server it was not saved for, and refused is what a 401 of it says; an error never quotes it.
func apiKey(remote *url.URL, saved Config) (key, refused string, err error) {
	key = strings.TrimSpace(os.Getenv(APIKeyEnv))
	if key != "" {
		if err := checkToken(key); err != nil {
			return "", "", fmt.Errorf("%s %w", APIKeyEnv, err)
		}

		return key, refusedKey(remote.String(), "the API key in "+APIKeyEnv, "set "+APIKeyEnv+" to a valid key"), nil
	}

	key = strings.TrimSpace(saved.APIKey)
	if key != "" && sameRemote(remote, saved.Remote) {
		path, err := ConfigPath(os.Getenv)
		if err != nil {
			return "", "", err
		}

		return key, refusedKey(remote.String(), "the API key saved in "+path, "run shard setup to replace it"), nil
	}
	if key != "" {
		return "", "", fmt.Errorf("the saved API key is for %s, so shard does not send it to %s; set %s, or run shard setup", Redacted(saved.Remote), remote.Redacted(), APIKeyEnv)
	}

	return "", "", fmt.Errorf("--remote needs an API key: set %s, or save one with shard setup", APIKeyEnv)
}

// sameRemote compares what the client dials, the scheme and the host and port, so a path or the case of a host name makes no other server.
func sameRemote(remote *url.URL, saved string) bool {
	other, err := url.Parse(saved)
	if err != nil {
		return false
	}

	return remote.Scheme == other.Scheme && strings.EqualFold(remoteAddress(remote), remoteAddress(other))
}

// Redacted is remote with the password it may carry hidden, as url.URL.Redacted prints it.
func Redacted(remote string) string {
	parsed, err := url.Parse(remote)
	if err != nil {
		return remote
	}

	return parsed.Redacted()
}

// checkToken refuses the bytes net/http refuses in a header, so a newline never splits a request; any other wrong token is the front's to refuse.
func checkToken(token string) error {
	if strings.ContainsFunc(token, func(r rune) bool { return r < ' ' && r != '\t' || r == 0x7f }) {
		return errors.New("holds a control character, which no HTTP header carries")
	}

	return nil
}
