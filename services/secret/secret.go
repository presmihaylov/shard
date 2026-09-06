// Package secret keeps the secrets a sandbox may use, on the host and never in a sandbox. A secret is
// granted to a destination, never to a sandbox alone: the proxy hands the value only to that host.
package secret

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/presmihaylov/shard/pkg/store"
)

// ErrNotFound is what a read of a secret the store does not hold returns. Match it with errors.Is.
var ErrNotFound = errors.New("secret not found")

const (
	// The store is its own directory, so a grep of the sandbox tree never finds a value.
	dirPerm  = 0o700
	filePerm = 0o600
	maxChars = 128
)

// Secret is what the store says about a secret. It never carries the value.
type Secret struct {
	Name string `json:"name"`
	// Destinations is where the value may go. A request to any other host never carries it.
	Destinations []string `json:"destinations"`
	// Placeholder is what the guest holds in place of the value, so it is not secret and it is printed.
	Placeholder string    `json:"placeholder"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Holders names the sandboxes that hold a grant on a secret, so a placeholder change knows who still sees it.
type Holders func(name string) ([]string, error)

// record is the file on disk. It is the only place the value is written.
type record struct {
	Value        string    `json:"value"`
	Destinations []string  `json:"destinations"`
	Placeholder  string    `json:"placeholder"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Store is the secret repository. One file per secret, mode 0600, under a directory nobody else
// may list.
type Store struct {
	dir     string
	holders Holders
}

// New prepares the store directory, which is <root>/secrets on the box. A nil holders cannot say who
// holds a placeholder, so such a store refuses to change one.
func New(dir string, holders Holders) (*Store, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("the secret store needs an absolute path, got %q", dir)
	}

	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}

	// MkdirAll leaves an existing directory's mode alone, and the mode is the whole protection.
	if err := os.Chmod(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("chmod %s: %w", dir, err)
	}

	return &Store{dir: dir, holders: holders}, nil
}

// Set writes the secret, or replaces the one of that name. A replace is the rotation: nothing
// caches a value, so a live sandbox uses the new one on its next request.
func (s *Store) Set(name, value string, destinations []string, placeholder string) (Secret, error) {
	if err := ValidName(name); err != nil {
		return Secret{}, err
	}

	if value == "" {
		return Secret{}, fmt.Errorf("secret %s has an empty value", name)
	}
	if strings.ContainsRune(value, 0) {
		return Secret{}, fmt.Errorf("secret %s holds a NUL byte, which no request header carries", name)
	}

	// A rotation names the value and nothing else: the grant and the placeholder it had stay.
	existing, err := s.read(name)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Secret{}, err
	}

	chosen, err := s.placeholder(name, value, placeholder, existing)
	if err != nil {
		return Secret{}, err
	}

	if len(destinations) == 0 {
		destinations = existing.Destinations
	}
	if len(destinations) == 0 {
		return Secret{}, fmt.Errorf("secret %s has no destination: a secret is granted to a host, never to a sandbox alone", name)
	}

	bound := make([]string, 0, len(destinations))
	for i, dest := range destinations {
		// The refusal names the position and never the value, so a list of destinations still says which one.
		canonical, err := validDestination(ordinal(i+1)+" destination", dest)
		if err != nil {
			return Secret{}, err
		}
		if !slices.Contains(bound, canonical) {
			bound = append(bound, canonical)
		}
	}

	rec := record{Value: value, Destinations: bound, Placeholder: chosen, UpdatedAt: time.Now().UTC()}

	blob, err := json.Marshal(rec)
	if err != nil {
		return Secret{}, fmt.Errorf("encode secret %s: %w", name, err)
	}

	if err := store.WriteFile(s.path(name), blob, filePerm); err != nil {
		return Secret{}, fmt.Errorf("write secret %s: %w", name, err)
	}

	return rec.public(name), nil
}

// placeholder settles what the guest will hold: the chosen one, else the stored one, else the default.
func (s *Store) placeholder(name, value, chosen string, existing record) (string, error) {
	if chosen == "" {
		chosen = existing.Placeholder
	}
	if chosen == "" {
		chosen = DefaultPlaceholder(name)
	}

	// A guest that held the value would need no proxy, so this rule holds for the default too.
	if strings.Contains(value, chosen) {
		return "", fmt.Errorf("the placeholder of secret %s is inside its value, and the guest must never hold the value", name)
	}

	// The default is exempt from the shape rules alone: a short NAME still gets a placeholder.
	if chosen != DefaultPlaceholder(name) {
		if err := shapedPlaceholder(name, chosen); err != nil {
			return "", err
		}
	}

	// Two secrets that share a placeholder substitute by list order, so this rule holds for the default too.
	if err := s.freePlaceholder(name, chosen); err != nil {
		return "", err
	}

	if existing.Placeholder != "" && chosen != existing.Placeholder {
		if err := s.placeholderMoved(name); err != nil {
			return "", err
		}
	}

	return chosen, nil
}

// minPlaceholder keeps a chosen placeholder long enough that it cannot fall inside an ordinary request.
const minPlaceholder = 8

// shapedPlaceholder refuses a chosen placeholder the proxy could not find in an ordinary request.
func shapedPlaceholder(name, chosen string) error {
	if len(chosen) < minPlaceholder {
		return fmt.Errorf("the placeholder of secret %s is shorter than %d characters", name, minPlaceholder)
	}
	for _, r := range chosen {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return fmt.Errorf("the placeholder of secret %s holds whitespace or a control character", name)
		}
	}

	return nil
}

// freePlaceholder refuses a placeholder another secret already owns, as its own or as its default.
func (s *Store) freePlaceholder(name, chosen string) error {
	stored, err := s.List()
	// A secret that does not read back may own the placeholder, so nothing can say it is free.
	if err != nil {
		return fmt.Errorf("the placeholder of secret %s cannot be told apart from the stored ones: %w", name, err)
	}

	for _, other := range stored {
		if other.Name == name {
			continue
		}
		if chosen == other.Placeholder || chosen == DefaultPlaceholder(other.Name) {
			return fmt.Errorf("the placeholder of secret %s is the placeholder of secret %s", name, other.Name)
		}
	}

	return nil
}

// placeholderMoved refuses to change what a running guest already holds: the placeholder moves only
// when no sandbox holds a grant on the secret.
func (s *Store) placeholderMoved(name string) error {
	if s.holders == nil {
		return fmt.Errorf("secret %s changes its placeholder and the store cannot tell which sandboxes hold it: ungrant it first", name)
	}

	holders, err := s.holders(name)
	if err != nil {
		return fmt.Errorf("secret %s changes its placeholder and the sandboxes that hold it cannot be read: %w", name, err)
	}
	if len(holders) != 0 {
		return fmt.Errorf("secret %s changes its placeholder and sandbox %s still holds it: ungrant it first", name, strings.Join(holders, ", "))
	}

	return nil
}

// Get reads what the store says about a secret, and never the value.
func (s *Store) Get(name string) (Secret, error) {
	rec, err := s.read(name)
	if err != nil {
		return Secret{}, err
	}

	return rec.public(name), nil
}

// Value reads the value. Only the proxy calls it, per request, so a rotation lands at once.
func (s *Store) Value(name string) (string, error) {
	rec, err := s.read(name)
	if err != nil {
		return "", err
	}

	return rec.Value, nil
}

// List names every secret, sorted, with its destinations and never a value.
func (s *Store) List() ([]Secret, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.dir, err)
	}

	var errs error
	secrets := make([]Secret, 0, len(entries))
	for _, entry := range entries {
		// The atomic write leaves a temp file behind on a crash, and a name it could not have is not a secret.
		if entry.IsDir() || ValidName(entry.Name()) != nil {
			continue
		}

		rec, err := s.read(entry.Name())
		if err != nil {
			// One broken file must not hide the rest, so the readable ones come back with the error.
			errs = errors.Join(errs, err)

			continue
		}

		secrets = append(secrets, rec.public(entry.Name()))
	}

	return secrets, errs
}

// Remove deletes the secret. It is idempotent: the store holding no such secret is the outcome asked for.
func (s *Store) Remove(name string) error {
	if err := ValidName(name); err != nil {
		return err
	}

	if err := os.Remove(s.path(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove secret %s: %w", name, err)
	}

	return store.SyncDir(s.dir)
}

func (s *Store) read(name string) (record, error) {
	if err := ValidName(name); err != nil {
		return record{}, err
	}

	blob, err := os.ReadFile(s.path(name))
	if errors.Is(err, fs.ErrNotExist) {
		return record{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if err != nil {
		return record{}, fmt.Errorf("read secret %s: %w", name, err)
	}

	// The decoder's error quotes a byte of the file, which may be a byte of the value, so it stays out.
	var rec record
	if err := json.Unmarshal(blob, &rec); err != nil {
		return record{}, fmt.Errorf("decode secret %s: the file is not valid JSON", name)
	}

	return rec, nil
}

func (s *Store) path(name string) string { return filepath.Join(s.dir, name) }

func (r record) public(name string) Secret {
	return Secret{Name: name, Destinations: slices.Clone(r.Destinations), Placeholder: r.Placeholder, UpdatedAt: r.UpdatedAt}
}

// DefaultPlaceholder is what the guest holds in place of the value when the operator names none.
func DefaultPlaceholder(name string) string { return "mock-" + name }

// A secret name is the environment variable the guest reads, so it is shaped like one.
var nameShape = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// ValidName refuses a name that is not an uppercase environment variable name.
func ValidName(name string) error {
	if name == "" {
		return errors.New("the secret name is empty")
	}
	// A mistyped set hands the value as the name, so a refusal never echoes what it refused.
	if len(name) > maxChars {
		return fmt.Errorf("the secret name is longer than %d characters", maxChars)
	}
	if !nameShape.MatchString(name) {
		return errors.New("the secret name is not an environment variable name: uppercase letters, digits and _, not starting with a digit")
	}

	return nil
}

// A destination is a host name, lowercase, with no scheme, no port and no path.
var labelShape = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// ValidDestination canonicalises a host name, so the proxy compares what a request names against
// one spelling. An address is refused: a secret goes to a name the proxy can see in the request.
func ValidDestination(dest string) (string, error) {
	return validDestination("destination", dest)
}

// validDestination phrases its refusals about subject, which a caller with several destinations
// makes an ordinal. A mistyped --to hands the value as one, so no refusal here echoes what it refused.
func validDestination(subject, dest string) (string, error) {
	canonical := strings.ToLower(strings.TrimSuffix(dest, "."))

	if canonical == "" {
		return "", fmt.Errorf("the %s is empty", subject)
	}
	if strings.ContainsAny(canonical, "/:") {
		return "", fmt.Errorf("the %s is a host name alone: no scheme, no port, no path", subject)
	}
	if _, err := netip.ParseAddr(canonical); err == nil {
		return "", fmt.Errorf("the %s is an address: a secret is granted to a host name", subject)
	}
	if len(canonical) > 253 {
		return "", fmt.Errorf("the %s is longer than a host name may be", subject)
	}

	labels := strings.Split(canonical, ".")
	if len(labels) < 2 {
		return "", fmt.Errorf("the %s has no dot: name the host the way a request does", subject)
	}
	for _, label := range labels {
		// A whole label may be a wildcard; api* would be an unmatchable shape.
		if label == "*" {
			continue
		}
		if strings.Contains(label, "*") {
			return "", fmt.Errorf("the %s puts * inside a label: a wildcard replaces a whole label", subject)
		}
		if !labelShape.MatchString(label) {
			return "", fmt.Errorf("the %s is not a host name", subject)
		}
	}

	return canonical, nil
}

// ordinal spells a position, so a refusal can say which destination it means without naming it.
func ordinal(n int) string {
	suffix := "th"
	switch {
	case n%100 >= 11 && n%100 <= 13:
	case n%10 == 1:
		suffix = "st"
	case n%10 == 2:
		suffix = "nd"
	case n%10 == 3:
		suffix = "rd"
	}

	return fmt.Sprintf("%d%s", n, suffix)
}
