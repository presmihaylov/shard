package serve

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// claims is the token payload: the standard fields, plus the scopes the front checks each request against.
type claims struct {
	Scopes []string `json:"scopes,omitempty"`
	jwt.RegisteredClaims
}

// Token is what serve mint prints and a client reads back: the signed token, its expiry and its scopes.
type Token struct {
	Token     string     `json:"token"`
	ExpiresAt *time.Time `json:"expires_at"`
	Scopes    []string   `json:"scopes"`
}

// Mint signs an HS256 token for sub, valid for ttl from now, carrying scopes; no scopes means every verb.
func Mint(secret []byte, sub string, scopes []string, ttl time.Duration) (string, error) {
	minted, err := MintToken(secret, sub, scopes, ttl)
	if err != nil {
		return "", err
	}

	return minted.Token, nil
}

// MintToken signs the token and answers it with the expiry and scopes it carries; empty scopes mints ["*"], every verb.
func MintToken(secret []byte, sub string, scopes []string, ttl time.Duration) (Token, error) {
	if sub == "" {
		return Token{}, errors.New("a token needs a subject")
	}
	if ttl <= 0 {
		return Token{}, fmt.Errorf("a token needs a duration in the future, got %s", ttl)
	}
	if len(scopes) == 0 {
		scopes = []string{"*"}
	}

	now := time.Now()
	exp := now.Add(ttl)
	c := claims{
		Scopes: scopes,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   sub,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(secret)
	if err != nil {
		return Token{}, fmt.Errorf("sign the token: %w", err)
	}

	// The record's expiry equals the token's exp to the second: both floor to whole seconds, in UTC.
	expUTC := exp.UTC().Truncate(time.Second)

	return Token{Token: signed, ExpiresAt: &expUTC, Scopes: scopes}, nil
}

// verify checks the token is HS256 over the secret, carries a subject and an issued-at, and has not expired, then answers the subject and the scopes it carries.
func verify(secret []byte, token string) (string, []string, error) {
	var c claims

	keyFunc := func(*jwt.Token) (any, error) { return secret, nil }
	if _, err := jwt.ParseWithClaims(token, &c, keyFunc,
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithExpirationRequired()); err != nil {
		return "", nil, fmt.Errorf("verify the token: %w", err)
	}

	if c.Subject == "" {
		return "", nil, errors.New("the token carries no subject")
	}
	if c.IssuedAt == nil {
		return "", nil, errors.New("the token carries no issued-at")
	}

	return c.Subject, c.Scopes, nil
}

// ReadSecret refuses a file others can read, because the secret signs and checks every token.
func ReadSecret(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("shard serve needs --secret-file: it holds the secret that signs and checks every token")
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read the secret file %s: %w", path, err)
	}
	if info.Mode().Perm()&0o007 != 0 {
		return nil, fmt.Errorf("the secret file %s is at mode %04o, which everyone on the host can read", path, info.Mode().Perm())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the secret file %s: %w", path, err)
	}

	secret := strings.TrimSpace(string(raw))
	if secret == "" {
		return nil, fmt.Errorf("the secret file %s holds no secret", path)
	}
	// RFC 7518 wants an HS256 key at least the hash width, 32 bytes, or an offline brute force breaks a short one.
	if len(secret) < 32 {
		return nil, fmt.Errorf("the secret in %s is %d bytes; the front needs at least 32: openssl rand -hex 32 > %s", path, len(secret), path)
	}

	return []byte(secret), nil
}
