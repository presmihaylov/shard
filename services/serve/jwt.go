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

// Token is what tokens mint prints and a client reads back: the signed token, its expiry and its scopes.
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
	c, err := newClaims(sub, scopes, ttl)
	if err != nil {
		return Token{}, err
	}

	return signClaims(secret, c)
}

// newClaims builds the payload: a random jti, the subject, an issued-at, the scopes, and an exp only when ttl is positive.
func newClaims(sub string, scopes []string, ttl time.Duration) (claims, error) {
	if sub == "" {
		return claims{}, errors.New("a token needs a subject")
	}
	if ttl < 0 {
		return claims{}, fmt.Errorf("a token needs a duration in the future, got %s", ttl)
	}
	if len(scopes) == 0 {
		scopes = []string{"*"}
	}

	jti, err := newJTI()
	if err != nil {
		return claims{}, err
	}

	now := time.Now()
	c := claims{
		Scopes: scopes,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:       jti,
			Subject:  sub,
			IssuedAt: jwt.NewNumericDate(now),
		},
	}
	// A zero ttl leaves off the exp claim, so the token never expires and the front stops enforcing exp on it.
	if ttl > 0 {
		c.ExpiresAt = jwt.NewNumericDate(now.Add(ttl))
	}

	return c, nil
}

// signClaims signs the claims and answers the printable record, whose expiry mirrors the token's exp to the second.
func signClaims(secret []byte, c claims) (Token, error) {
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(secret)
	if err != nil {
		return Token{}, fmt.Errorf("sign the token: %w", err)
	}

	return Token{Token: signed, ExpiresAt: recordExpiry(c), Scopes: c.Scopes}, nil
}

// recordExpiry is the claims' exp floored to the second in UTC, or nil when the token never expires.
func recordExpiry(c claims) *time.Time {
	if c.ExpiresAt == nil {
		return nil
	}

	exp := c.ExpiresAt.Time.UTC().Truncate(time.Second)

	return &exp
}

// verify checks the token is HS256 over the secret, carries a subject, an issued-at and a jti, and enforces an exp when present, then answers the subject, the scopes and the jti.
func verify(secret []byte, token string) (string, []string, string, error) {
	var c claims

	keyFunc := func(*jwt.Token) (any, error) { return secret, nil }
	if _, err := jwt.ParseWithClaims(token, &c, keyFunc,
		jwt.WithValidMethods([]string{"HS256"})); err != nil {
		return "", nil, "", fmt.Errorf("verify the token: %w", err)
	}

	if c.Subject == "" {
		return "", nil, "", errors.New("the token carries no subject")
	}
	if c.IssuedAt == nil {
		return "", nil, "", errors.New("the token carries no issued-at")
	}
	if c.ID == "" {
		return "", nil, "", errors.New("the token carries no id")
	}

	return c.Subject, c.Scopes, c.ID, nil
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
