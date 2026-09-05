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
	// MockValue is what the guest sees in its environment. Its shape can matter to an SDK.
	MockValue string    `json:"mock_value"`
	UpdatedAt time.Time `json:"updated_at"`
	// Headers is what the proxy sets on a granted request, over anything the guest sent.
	Headers []Header `json:"headers,omitempty"`
	// Match limits which granted requests get the headers; nil is every one.
	Match *Match `json:"match,omitempty"`
}

// Header is one header the proxy sets; {value} in the template becomes the secret value.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Pair is one name and value a match compares against.
type Pair struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Match is what a request must meet, every set dimension at once. A path is exact, a trailing *
// makes it a prefix, and re: makes it an RE2 expression. Methods is any-of; Query and Headers all-of.
type Match struct {
	Path    string   `json:"path,omitempty"`
	Methods []string `json:"methods,omitempty"`
	Query   []Pair   `json:"query,omitempty"`
	Headers []Pair   `json:"headers,omitempty"`
}

// Update is what one secret set names. An empty field keeps what the store holds.
type Update struct {
	Destinations []string
	Headers      []Header
	Match        *Match
	// Holders names the sandboxes granted the secret. Set asks only when the stored placeholder differs from the one it writes.
	Holders func() ([]string, error)
}

// HeldPlaceholder is the refusal a rotation gets while a sandbox holds a placeholder the new record would not carry.
type HeldPlaceholder struct {
	Name    string
	Holders []string
}

func (e HeldPlaceholder) Error() string {
	return fmt.Sprintf("secret %s keeps the placeholder sandbox %s was created with, and a rotation would move it: ungrant the sandbox first", e.Name, strings.Join(e.Holders, ", "))
}

// record is the file on disk. It is the only place the value is written.
type record struct {
	Value        string    `json:"value"`
	Destinations []string  `json:"destinations"`
	MockValue    string    `json:"mock_value"`
	UpdatedAt    time.Time `json:"updated_at"`
	Headers      []Header  `json:"headers,omitempty"`
	Match        *Match    `json:"match,omitempty"`
}

// Store is the secret repository. One file per secret, mode 0600, under a directory nobody else
// may list.
type Store struct {
	dir string
}

// New prepares the store directory, which is <root>/secrets on the box.
func New(dir string) (*Store, error) {
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

	return &Store{dir: dir}, nil
}

// Set writes the secret, or replaces the one of that name. A replace is the rotation: nothing
// caches a value, so a live sandbox uses the new one on its next request.
func (s *Store) Set(name, value string, up Update) (Secret, error) {
	destinations := up.Destinations

	if err := ValidName(name); err != nil {
		return Secret{}, err
	}

	if value == "" {
		return Secret{}, fmt.Errorf("secret %s has an empty value", name)
	}
	if strings.ContainsRune(value, 0) {
		return Secret{}, fmt.Errorf("secret %s holds a NUL byte, which no request header carries", name)
	}

	// A rotation names the value and nothing else: the grant, the headers and the match it had stay.
	existing, err := s.read(name)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Secret{}, err
	}
	if len(destinations) == 0 {
		destinations = existing.Destinations
	}
	headers := up.Headers
	if headers == nil {
		headers = existing.Headers
	}
	match := up.Match
	if match == nil {
		match = existing.Match
	}
	if err := validHeaders(name, headers); err != nil {
		return Secret{}, err
	}
	if err := validMatch(name, match); err != nil {
		return Secret{}, err
	}

	if len(destinations) == 0 {
		return Secret{}, fmt.Errorf("secret %s has no destination: a secret is granted to a host, never to a sandbox alone", name)
	}

	bound := make([]string, 0, len(destinations))
	for _, dest := range destinations {
		canonical, err := ValidDestination(dest)
		if err != nil {
			return Secret{}, err
		}
		if !slices.Contains(bound, canonical) {
			bound = append(bound, canonical)
		}
	}

	mock := MockValue(name)
	if strings.Contains(value, mock) {
		return Secret{}, fmt.Errorf("the value of secret %s holds its placeholder, and the guest must never hold the value", name)
	}

	// A guest sends the placeholder it was created with, so a record that moves it leaves that guest unmatched.
	if err == nil && existing.MockValue != mock {
		if err := placeholderFree(name, up.Holders); err != nil {
			return Secret{}, err
		}
	}

	rec := record{Value: value, Destinations: bound, MockValue: mock, UpdatedAt: time.Now().UTC(), Headers: headers, Match: match}

	blob, err := json.Marshal(rec)
	if err != nil {
		return Secret{}, fmt.Errorf("encode secret %s: %w", name, err)
	}

	if err := store.WriteFile(s.path(name), blob, filePerm); err != nil {
		return Secret{}, fmt.Errorf("write secret %s: %w", name, err)
	}

	return rec.public(name), nil
}

func placeholderFree(name string, holders func() ([]string, error)) error {
	if holders == nil {
		return fmt.Errorf("secret %s keeps an older placeholder, and nothing says which sandboxes hold it", name)
	}

	held, err := holders()
	if err != nil {
		return fmt.Errorf("cannot tell which sandboxes hold secret %s: %w", name, err)
	}
	if len(held) != 0 {
		return HeldPlaceholder{Name: name, Holders: held}
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
	return Secret{Name: name, Destinations: slices.Clone(r.Destinations), MockValue: r.MockValue, UpdatedAt: r.UpdatedAt, Headers: slices.Clone(r.Headers), Match: r.Match}
}

// MockValue is the placeholder the guest sees in place of the value.
func MockValue(name string) string { return "mock-" + name }

// A secret name is the environment variable the guest reads, so it is shaped like one.
var nameShape = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// ValidName refuses a name that is not an uppercase environment variable name.
func ValidName(name string) error {
	if name == "" {
		return errors.New("the secret name is empty")
	}
	if len(name) > maxChars {
		return fmt.Errorf("the secret name %q is longer than %d characters", name, maxChars)
	}
	if !nameShape.MatchString(name) {
		return fmt.Errorf("the secret name %q is not an environment variable name: uppercase letters, digits and _, not starting with a digit", name)
	}

	return nil
}

// A destination is a host name, lowercase, with no scheme, no port and no path.
var labelShape = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// ValidDestination canonicalises a host name, so the proxy compares what a request names against
// one spelling. An address is refused: a secret goes to a name the proxy can see in the request.
func ValidDestination(dest string) (string, error) {
	canonical := strings.ToLower(strings.TrimSuffix(dest, "."))

	if canonical == "" {
		return "", errors.New("the destination is empty")
	}
	if strings.ContainsAny(canonical, "/:") {
		return "", fmt.Errorf("the destination %q is a host name alone: no scheme, no port, no path", dest)
	}
	if _, err := netip.ParseAddr(canonical); err == nil {
		return "", fmt.Errorf("the destination %q is an address: a secret is granted to a host name", dest)
	}
	if len(canonical) > 253 {
		return "", fmt.Errorf("the destination %q is longer than a host name may be", dest)
	}

	labels := strings.Split(canonical, ".")
	if len(labels) < 2 {
		return "", fmt.Errorf("the destination %q has no dot: name the host the way a request does", dest)
	}
	for _, label := range labels {
		// A whole label may be a wildcard; api* would be an unmatchable shape.
		if label == "*" {
			continue
		}
		if strings.Contains(label, "*") {
			return "", fmt.Errorf("the destination %q puts * inside a label: a wildcard replaces a whole label", dest)
		}
		if !labelShape.MatchString(label) {
			return "", fmt.Errorf("the destination %q is not a host name", dest)
		}
	}

	return canonical, nil
}

// headerName is a field name as HTTP spells one.
var headerName = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

func validHeaders(name string, headers []Header) error {
	seen := make([]string, 0, len(headers))
	for _, h := range headers {
		lower := strings.ToLower(h.Name)
		if !headerName.MatchString(h.Name) {
			return fmt.Errorf("secret %s header %q is not a header name", name, h.Name)
		}
		if slices.Contains(seen, lower) {
			return fmt.Errorf("secret %s names the header %s twice", name, h.Name)
		}
		seen = append(seen, lower)
		if strings.ContainsAny(h.Value, "\r\n\x00") {
			return fmt.Errorf("secret %s header %s holds a control character", name, h.Name)
		}
	}

	return nil
}

func validMatch(name string, match *Match) error {
	if match == nil {
		return nil
	}
	if expr, ok := strings.CutPrefix(match.Path, "re:"); ok {
		if _, err := regexp.Compile(expr); err != nil {
			return fmt.Errorf("secret %s match path: %w", name, err)
		}
	}
	if slices.Contains(match.Methods, "") {
		return fmt.Errorf("secret %s match names an empty method", name)
	}
	unnamed := func(pair Pair) bool { return pair.Name == "" }
	if slices.ContainsFunc(slices.Concat(match.Query, match.Headers), unnamed) {
		return fmt.Errorf("secret %s match holds a condition with no name", name)
	}

	return nil
}
