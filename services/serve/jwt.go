package serve

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Mint signs an HS256 token for sub, valid for ttl from now. The secret is the raw bytes of the file.
func Mint(secret []byte, sub string, ttl time.Duration) (string, error) {
	if sub == "" {
		return "", errors.New("a token needs a subject")
	}
	if ttl <= 0 {
		return "", fmt.Errorf("a token needs a duration in the future, got %s", ttl)
	}

	now := time.Now()
	claims := jwt.RegisteredClaims{
		Subject:   sub,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
	if err != nil {
		return "", fmt.Errorf("sign the token: %w", err)
	}

	return signed, nil
}

// verify checks the token is HS256 over the secret, carries a subject and an issued-at, and has not expired.
func verify(secret []byte, token string) (string, error) {
	var claims jwt.RegisteredClaims

	keyFunc := func(*jwt.Token) (any, error) { return secret, nil }
	if _, err := jwt.ParseWithClaims(token, &claims, keyFunc,
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithExpirationRequired()); err != nil {
		return "", fmt.Errorf("verify the token: %w", err)
	}

	if claims.Subject == "" {
		return "", errors.New("the token carries no subject")
	}
	if claims.IssuedAt == nil {
		return "", errors.New("the token carries no issued-at")
	}

	return claims.Subject, nil
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

	return []byte(secret), nil
}
