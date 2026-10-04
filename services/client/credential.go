package client

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"strings"
)

// The environment a remote client reads, as docker's reads DOCKER_HOST. The CLI and NewRemoteFromEnv share it.
const (
	RemoteEnv = "SHARD_REMOTE"
	APIKeyEnv = "SHARD_API_KEY" //nolint:gosec // G101: this names the variable, and holds no key
	CAFileEnv = "SHARD_CA_FILE"
)

// NewRemoteFromEnv builds the client of host, or of SHARD_REMOTE when host is empty, with SHARD_API_KEY and SHARD_CA_FILE.
func NewRemoteFromEnv(host string) (*Client, error) {
	host = cmp.Or(host, os.Getenv(RemoteEnv))
	if host == "" {
		return nil, fmt.Errorf("a remote client needs a host: --remote or %s, as https://shard.example.com", RemoteEnv)
	}
	parsed, err := parseRemote(host)
	if err != nil {
		return nil, err
	}

	token, err := apiKey()
	if err != nil {
		return nil, err
	}

	caFile := os.Getenv(CAFileEnv)
	if caFile == "" {
		return NewRemote(host, token, nil)
	}
	// Refused before the file is read, so the mismatch is the one error, whatever the file holds.
	if parsed.Scheme == "http" {
		return nil, fmt.Errorf("%s is set, and the remote %s is http: a CA certificate verifies an https remote only", CAFileEnv, host)
	}
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read the ca file %s from %s: %w", caFile, CAFileEnv, err)
	}

	return NewRemote(host, token, ca)
}

// apiKey is SHARD_API_KEY, the one credential of a remote client; an error names the variable, never the value.
func apiKey() (string, error) {
	key := strings.TrimSpace(os.Getenv(APIKeyEnv))
	if key == "" {
		return "", fmt.Errorf("a remote client needs %s; shard serve answers 401 without one", APIKeyEnv)
	}
	if err := checkToken(key); err != nil {
		return "", fmt.Errorf("%s %w", APIKeyEnv, err)
	}

	return key, nil
}

// checkToken refuses the bytes net/http refuses in a header, so a newline never splits a request; any other wrong token is the front's to refuse.
func checkToken(token string) error {
	if strings.ContainsFunc(token, func(r rune) bool { return r < ' ' && r != '\t' || r == 0x7f }) {
		return errors.New("holds a control character, which no HTTP header carries")
	}

	return nil
}
