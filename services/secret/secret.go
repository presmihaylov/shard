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
	// Headers is what the proxy sets on a granted request, over whatever the guest sent.
	Headers []Header `json:"headers,omitempty"`
	// Match gates the headers alone; the placeholder is replaced on every granted request.
	Match     Match     `json:"match,omitzero"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Header is one header the proxy sets; {value} in Value expands to the secret value on the way out.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Match is every condition a request must meet for the headers to be set; an empty one meets every request.
type Match struct {
	Path   string `json:"path,omitempty"`
	Method string `json:"method,omitempty"`
	// Query and Headers are key=value pairs the request must carry, all of them.
	Query   []string `json:"query,omitempty"`
	Headers []string `json:"headers,omitempty"`
}

// Holders names the sandboxes that hold a grant on a secret, so a rotation knows who still sees its placeholder.
type Holders func(name string) ([]string, error)

// record is the file on disk. It is the only place the value is written.
type record struct {
	Value        string    `json:"value"`
	Destinations []string  `json:"destinations"`
	MockValue    string    `json:"mock_value"`
	Headers      []Header  `json:"headers,omitempty"`
	Match        Match     `json:"match,omitzero"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Store is the secret repository. One file per secret, mode 0600, under a directory nobody else
// may list.
type Store struct {
	dir     string
	holders Holders
}

// New prepares the store directory, which is <root>/secrets on the box. A nil holders cannot say who
// sees an old placeholder, so such a store refuses to rotate a secret that has one.
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
func (s *Store) Set(name, value string, destinations []string, headers []Header, match Match) (Secret, error) {
	if err := ValidName(name); err != nil {
		return Secret{}, err
	}

	if value == "" {
		return Secret{}, fmt.Errorf("secret %s has an empty value", name)
	}
	if strings.ContainsRune(value, 0) {
		return Secret{}, fmt.Errorf("secret %s holds a NUL byte, which no request header carries", name)
	}
	if strings.Contains(value, MockValue(name)) {
		return Secret{}, fmt.Errorf("the placeholder of secret %s is inside its value, and the guest must never hold the value", name)
	}

	// A rotation names the value and nothing else: the grant and the headers it had stay.
	existing, err := s.read(name)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Secret{}, err
	}
	if err := s.placeholderMoved(name, existing); err != nil {
		return Secret{}, err
	}
	if len(destinations) == 0 {
		destinations = existing.Destinations
	}
	if headers == nil {
		headers = existing.Headers
	}
	if match.empty() {
		match = existing.Match
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

	for _, header := range headers {
		if err := validHeader(header); err != nil {
			return Secret{}, fmt.Errorf("secret %s: %w", name, err)
		}
	}
	if err := validMatch(match); err != nil {
		return Secret{}, fmt.Errorf("secret %s: %w", name, err)
	}

	rec := record{Value: value, Destinations: bound, MockValue: MockValue(name), Headers: headers, Match: match, UpdatedAt: time.Now().UTC()}

	blob, err := json.Marshal(rec)
	if err != nil {
		return Secret{}, fmt.Errorf("encode secret %s: %w", name, err)
	}

	if err := store.WriteFile(s.path(name), blob, filePerm); err != nil {
		return Secret{}, fmt.Errorf("write secret %s: %w", name, err)
	}

	return rec.public(name), nil
}

// placeholderMoved refuses to change what a running guest already holds: a record written with another
// placeholder is rotated only when no sandbox holds a grant on it.
func (s *Store) placeholderMoved(name string, existing record) error {
	if existing.MockValue == "" || existing.MockValue == MockValue(name) {
		return nil
	}
	if s.holders == nil {
		return fmt.Errorf("secret %s has an old placeholder and the store cannot tell which sandboxes hold it: ungrant it first", name)
	}

	holders, err := s.holders(name)
	if err != nil {
		return fmt.Errorf("secret %s has an old placeholder and the sandboxes that hold it cannot be read: %w", name, err)
	}
	if len(holders) != 0 {
		return fmt.Errorf("secret %s has an old placeholder that sandbox %s still holds: ungrant it first", name, strings.Join(holders, ", "))
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
	return Secret{Name: name, Destinations: slices.Clone(r.Destinations), Headers: slices.Clone(r.Headers), Match: r.Match, UpdatedAt: r.UpdatedAt}
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

// ParseHeader reads the --header spelling, "Name: value", where value may hold {value}.
func ParseHeader(text string) (Header, error) {
	name, value, ok := strings.Cut(text, ":")
	if !ok {
		return Header{}, fmt.Errorf("the header %q is not \"Name: value\"", text)
	}

	header := Header{Name: strings.TrimSpace(name), Value: strings.TrimSpace(value)}
	if err := validHeader(header); err != nil {
		return Header{}, err
	}

	return header, nil
}

// A header name is an HTTP token.
var tokenShape = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_` + "`" + `|~-]+$`)

func validHeader(header Header) error {
	if !tokenShape.MatchString(header.Name) {
		return fmt.Errorf("the header name %q is not a header name", header.Name)
	}
	if header.Value == "" {
		return fmt.Errorf("the header %s has no value", header.Name)
	}
	if strings.ContainsAny(header.Value, "\r\n\x00") {
		return fmt.Errorf("the header %s holds a line break", header.Name)
	}

	return nil
}

// ParseMatch reads the --match spellings: path=/prefix, method=POST, query=key=value and header=Name=value.
func ParseMatch(items []string) (Match, error) {
	var match Match
	for _, item := range items {
		key, value, ok := strings.Cut(item, "=")
		if !ok || value == "" {
			return Match{}, fmt.Errorf("the match %q is not key=value", item)
		}

		switch key {
		case "path":
			match.Path = value
		case "method":
			match.Method = strings.ToUpper(value)
		case "query":
			match.Query = append(match.Query, value)
		case "header":
			match.Headers = append(match.Headers, value)
		default:
			return Match{}, fmt.Errorf("the match %q names %s, which is none of path, method, query and header", item, key)
		}
	}

	if err := validMatch(match); err != nil {
		return Match{}, err
	}

	return match, nil
}

func validMatch(match Match) error {
	if match.Path != "" && !strings.HasPrefix(match.Path, "/") {
		return fmt.Errorf("the match path %q does not start with /", match.Path)
	}
	if match.Method != "" && !tokenShape.MatchString(match.Method) {
		return fmt.Errorf("the match method %q is not a method", match.Method)
	}
	for _, pair := range append(slices.Clone(match.Query), match.Headers...) {
		key, _, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			return fmt.Errorf("the match %q is not key=value", pair)
		}
	}

	return nil
}

func (m Match) empty() bool {
	return m.Path == "" && m.Method == "" && len(m.Query) == 0 && len(m.Headers) == 0
}
