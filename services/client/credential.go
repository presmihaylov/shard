package client

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// The environment a remote client reads, as docker's reads DOCKER_HOST. The CLI and NewRemoteFromEnv share it.
const (
	RemoteEnv    = "SHARD_REMOTE"
	APIKeyEnv    = "SHARD_API_KEY"    //nolint:gosec // G101: this names the variable, and holds no key
	TokenFileEnv = "SHARD_TOKEN_FILE" //nolint:gosec // G101: this names a file, and holds no token
	CAFileEnv    = "SHARD_CA_FILE"
)

// RemoteOptions are what a caller names itself; each empty field falls back to the environment.
type RemoteOptions struct {
	// Host is the shard serve front, as https://box.example.com:2376; empty reads SHARD_REMOTE.
	Host string
	// TokenFile is an explicit token file, as the CLI's --token-file, and beats SHARD_API_KEY and SHARD_TOKEN_FILE.
	TokenFile string
	// CAFile signed the front's certificate; empty reads SHARD_CA_FILE, and empty there leaves the host's trust store.
	CAFile string
}

// NewRemoteFromEnv builds the client of the front the options name, and the environment where they name nothing.
func NewRemoteFromEnv(opts RemoteOptions) (*Client, error) {
	host := cmp.Or(opts.Host, os.Getenv(RemoteEnv))
	if host == "" {
		return nil, fmt.Errorf("a remote client needs a host: --remote or %s, as https://box.example.com:2376", RemoteEnv)
	}

	token, err := ResolveToken(opts.TokenFile)
	if err != nil {
		return nil, err
	}

	var ca []byte
	if caFile := cmp.Or(opts.CAFile, os.Getenv(CAFileEnv)); caFile != "" {
		ca, err = os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read the ca file %s: %w", caFile, err)
		}
	}

	return NewRemote(host, token, ca)
}

// ResolveToken answers the token of the first source set, tokenFile then SHARD_API_KEY then SHARD_TOKEN_FILE; an error names the source, never the value.
func ResolveToken(tokenFile string) (string, error) {
	if tokenFile != "" {
		return readTokenFile(tokenFile)
	}

	// A blank key is unset, as an empty one is, so a stray export never shadows SHARD_TOKEN_FILE.
	if key := strings.TrimSpace(os.Getenv(APIKeyEnv)); key != "" {
		if err := checkToken(key); err != nil {
			return "", fmt.Errorf("%s %w", APIKeyEnv, err)
		}

		return key, nil
	}

	if path := os.Getenv(TokenFileEnv); path != "" {
		token, err := readTokenFile(path)
		if err != nil {
			return "", fmt.Errorf("%s: %w", TokenFileEnv, err)
		}

		return token, nil
	}

	return "", fmt.Errorf("a remote client needs a token: --token-file, %s or %s, in that order; shard serve answers 401 without one", APIKeyEnv, TokenFileEnv)
}

// readTokenFile refuses a file others can read, because the token is the whole of the authentication.
func readTokenFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("read the token file %s: %w", path, err)
	}
	if info.Mode().Perm()&0o007 != 0 {
		return "", fmt.Errorf("the token file %s is at mode %04o, which everyone on the host can read", path, info.Mode().Perm())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read the token file %s: %w", path, err)
	}

	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("the token file %s holds no token", path)
	}

	// tokens mint writes a JSON record; a token file holds it whole or the bare token.
	if strings.HasPrefix(token, "{") {
		var record struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal([]byte(token), &record); err != nil {
			return "", fmt.Errorf("parse the mint record in the token file %s: %w", path, err)
		}
		token = strings.TrimSpace(record.Token)
		if token == "" {
			return "", fmt.Errorf("the mint record in the token file %s holds no token", path)
		}
	}

	if err := checkToken(token); err != nil {
		return "", fmt.Errorf("the token in %s %w", path, err)
	}

	return token, nil
}

// checkToken refuses the bytes net/http refuses in a header, so a newline never splits a request; any other wrong token is the front's to refuse.
func checkToken(token string) error {
	if strings.ContainsFunc(token, func(r rune) bool { return r < ' ' && r != '\t' || r == 0x7f }) {
		return errors.New("holds a control character, which no HTTP header carries")
	}

	return nil
}
