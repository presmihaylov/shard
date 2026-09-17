package serve

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/presmihaylov/shard/pkg/store"
)

// TokensFileName is the ledger the front reads and mint appends to, beside the secret file.
const TokensFileName = "serve.tokens"

// ledgerEntry is one minted token's record: the front matches a request's jti against it, and revoke flips Revoked.
type ledgerEntry struct {
	JTI       string     `json:"jti"`
	Sub       string     `json:"sub"`
	IssuedAt  time.Time  `json:"issued_at"`
	ExpiresAt *time.Time `json:"expires_at"`
	Scopes    []string   `json:"scopes"`
	Revoked   bool       `json:"revoked"`
}

// TokenStatus is how serve tokens reads a record now: active, revoked or past its expiry.
type TokenStatus string

const (
	StatusActive  TokenStatus = "active"
	StatusRevoked TokenStatus = "revoked"
	StatusExpired TokenStatus = "expired"
)

// TokenInfo is one ledger record as serve tokens prints it.
type TokenInfo struct {
	ID        string
	Subject   string
	IssuedAt  time.Time
	ExpiresAt *time.Time
	Scopes    []string
	Status    TokenStatus
}

// TokensPath is the ledger beside the secret file, or override when it is not empty.
func TokensPath(secretFile, override string) string {
	if override != "" {
		return override
	}

	return filepath.Join(filepath.Dir(secretFile), TokensFileName)
}

// IssueToken signs a token for sub, appends its record to the ledger at path, and answers the printable record.
// It answers no token when it cannot write the ledger, so every token it answers is recorded there.
func IssueToken(secret []byte, path, sub string, scopes []string, ttl time.Duration) (Token, error) {
	c, err := newClaims(sub, scopes, ttl)
	if err != nil {
		return Token{}, err
	}

	minted, err := signClaims(secret, c)
	if err != nil {
		return Token{}, err
	}

	entry := ledgerEntry{
		JTI:       c.ID,
		Sub:       c.Subject,
		IssuedAt:  c.IssuedAt.Time.UTC().Truncate(time.Second),
		ExpiresAt: recordExpiry(c),
		Scopes:    c.Scopes,
	}
	if err := appendEntry(path, entry); err != nil {
		return Token{}, err
	}

	return minted, nil
}

// ListTokens reads the ledger at path and answers each record with the status a request would see now.
func ListTokens(path string) ([]TokenInfo, error) {
	entries, err := readEntries(path)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	infos := make([]TokenInfo, 0, len(entries))
	for _, e := range entries {
		infos = append(infos, TokenInfo{
			ID:        e.JTI,
			Subject:   e.Sub,
			IssuedAt:  e.IssuedAt,
			ExpiresAt: e.ExpiresAt,
			Scopes:    e.Scopes,
			Status:    statusOf(e, now),
		})
	}

	return infos, nil
}

// RevokeToken marks the token with id revoked in the ledger at path, and answers how many records it matched.
func RevokeToken(path, id string) (int, error) {
	return revoke(path, func(e ledgerEntry) bool { return e.JTI == id })
}

// RevokeSubject marks every token of sub revoked in the ledger at path, and answers how many records it matched.
func RevokeSubject(path, sub string) (int, error) {
	return revoke(path, func(e ledgerEntry) bool { return e.Sub == sub })
}

// revoke flips every record match reports and not yet revoked, then rewrites the ledger, and answers the match count.
func revoke(path string, match func(ledgerEntry) bool) (int, error) {
	entries, err := readEntries(path)
	if err != nil {
		return 0, err
	}

	found, flipped := 0, 0
	for i := range entries {
		if !match(entries[i]) {
			continue
		}
		found++
		if !entries[i].Revoked {
			entries[i].Revoked = true
			flipped++
		}
	}
	if flipped == 0 {
		return found, nil
	}

	if err := writeEntries(path, entries); err != nil {
		return found, err
	}

	return found, nil
}

func statusOf(e ledgerEntry, now time.Time) TokenStatus {
	if e.Revoked {
		return StatusRevoked
	}
	if e.ExpiresAt != nil && now.After(*e.ExpiresAt) {
		return StatusExpired
	}

	return StatusActive
}

// newJTI is a random 128-bit id, hex, that names one token in the ledger.
func newJTI() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("read random bytes for the token id: %w", err)
	}

	return hex.EncodeToString(raw[:]), nil
}

// appendEntry adds one record to the ledger, and creates it 0640 when absent, so every minted token is recorded.
func appendEntry(path string, e ledgerEntry) error {
	if err := checkTokensMode(path); err != nil {
		return err
	}

	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encode the ledger record: %w", err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640) // #nosec G302: the front runs as another user and needs the group read; checkTokensMode refuses world read
	if err != nil {
		return fmt.Errorf("open the ledger %s: %w", path, err)
	}

	// Chmod defeats a umask that would trim the group read the front needs.
	if err := f.Chmod(0o640); err != nil {
		return errors.Join(fmt.Errorf("set the mode of the ledger %s: %w", path, err), f.Close())
	}

	if _, err := f.Write(append(line, '\n')); err != nil {
		return errors.Join(fmt.Errorf("append to the ledger %s: %w", path, err), f.Close())
	}

	return f.Close()
}

// readEntries reads every record from the ledger; a missing file is an empty ledger, not an error.
func readEntries(path string) ([]ledgerEntry, error) {
	if err := checkTokensMode(path); err != nil {
		return nil, err
	}

	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open the ledger %s: %w", path, err)
	}
	defer f.Close()

	var entries []ledgerEntry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		var e ledgerEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("parse a record in the ledger %s: %w", path, err)
		}
		entries = append(entries, e)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read the ledger %s: %w", path, err)
	}

	return entries, nil
}

// writeEntries rewrites the whole ledger atomically, which revoke does to flip one record in place.
func writeEntries(path string, entries []ledgerEntry) error {
	var buf bytes.Buffer
	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("encode a ledger record: %w", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}

	if err := store.WriteFile(path, buf.Bytes(), 0o640); err != nil {
		return fmt.Errorf("write the ledger %s: %w", path, err)
	}

	return nil
}

// checkTokensMode refuses a ledger others can read; a missing file is fine, because the caller creates it 0640.
func checkTokensMode(path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the ledger %s: %w", path, err)
	}
	if info.Mode().Perm()&0o007 != 0 {
		return fmt.Errorf("the ledger %s is at mode %04o, which everyone on the host can read", path, info.Mode().Perm())
	}

	return nil
}

// ledger is the front's view of the tokens file; it reloads when the file's mtime or size changes.
type ledger struct {
	path string

	mu      sync.RWMutex
	loaded  bool
	modTime time.Time
	size    int64
	byJTI   map[string]ledgerEntry
}

// newLedger builds the front's ledger and loads it once, so an unreadable ledger refuses to start the front.
func newLedger(path string) (*ledger, error) {
	l := &ledger{path: path, byJTI: map[string]ledgerEntry{}}
	if err := l.refresh(); err != nil {
		return nil, err
	}

	return l, nil
}

// refresh reloads the ledger when its file changed; a file that was there and vanished refuses every request.
func (l *ledger) refresh() error {
	info, err := os.Stat(l.path)
	if errors.Is(err, os.ErrNotExist) {
		l.mu.RLock()
		loaded := l.loaded
		l.mu.RUnlock()
		if loaded {
			return fmt.Errorf("the ledger %s is gone", l.path)
		}

		return nil
	}
	if err != nil {
		return fmt.Errorf("stat the ledger %s: %w", l.path, err)
	}

	l.mu.RLock()
	fresh := l.loaded && info.ModTime().Equal(l.modTime) && info.Size() == l.size
	l.mu.RUnlock()
	if fresh {
		return nil
	}

	entries, err := readEntries(l.path)
	if err != nil {
		return err
	}

	l.mu.Lock()
	l.byJTI = index(entries)
	l.modTime = info.ModTime()
	l.size = info.Size()
	l.loaded = true
	l.mu.Unlock()

	return nil
}

// lookup answers the record for jti and whether the ledger holds it.
func (l *ledger) lookup(jti string) (ledgerEntry, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	e, ok := l.byJTI[jti]

	return e, ok
}

// index maps entries by jti; revoke rewrites in place, so each jti appears once.
func index(entries []ledgerEntry) map[string]ledgerEntry {
	m := make(map[string]ledgerEntry, len(entries))
	for _, e := range entries {
		m[e.JTI] = e
	}

	return m
}
