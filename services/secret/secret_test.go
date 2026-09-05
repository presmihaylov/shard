package secret

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "secrets")

	s, err := New(dir, func(string) ([]string, error) { return nil, nil })
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return s, dir
}

func TestSetWritesOneFileNobodyElseCanRead(t *testing.T) {
	s, dir := newStore(t)

	sec, err := s.Set("API_KEY", "sk-live-1234567890", []string{"API.Example.com.", "api.example.com"}, nil, Match{})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}

	if got, want := sec.Destinations, []string{"api.example.com"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("destinations %v, want the one canonical host %v", got, want)
	}
	if MockValue("API_KEY") != "mock-API_KEY" {
		t.Errorf("placeholder %q, want mock-API_KEY", MockValue("API_KEY"))
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat the store: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("the store directory is %o, want 0700", got)
	}

	info, err = os.Stat(filepath.Join(dir, "API_KEY"))
	if err != nil {
		t.Fatalf("stat the secret: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("the secret file is %o, want 0600", got)
	}

	value, err := s.Value("API_KEY")
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if value != "sk-live-1234567890" {
		t.Errorf("Value read back %q", value)
	}
}

func TestNewTightensAnExistingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := New(dir, nil); err != nil {
		t.Fatalf("New: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("an existing store directory was left at %o, want 0700", got)
	}
}

func TestGetAndListNeverCarryTheValue(t *testing.T) {
	s, _ := newStore(t)

	const value = "hunter2-hunter2-hunter2"
	if _, err := s.Set("TOKEN", value, []string{"example.com"}, nil, Match{}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	sec, err := s.Get("TOKEN")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if strings.Contains(sec.Name+strings.Join(sec.Destinations, ""), value) {
		t.Errorf("Get carried the value: %+v", sec)
	}

	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Name != "TOKEN" {
		t.Fatalf("List = %+v, want the one secret", list)
	}
}

func TestSetReplacesAndRemoveIsIdempotent(t *testing.T) {
	s, _ := newStore(t)

	if _, err := s.Set("TOKEN", "first-value-1", []string{"a.example.com"}, nil, Match{}); err != nil {
		t.Fatal(err)
	}
	headers := []Header{{Name: "Authorization", Value: "Bearer {value}"}}
	if _, err := s.Set("TOKEN", "second-value-2", []string{"b.example.com"}, headers, Match{Path: "/v1/"}); err != nil {
		t.Fatal(err)
	}

	sec, err := s.Get("TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sec.Destinations, ",") != "b.example.com" || !slices.Equal(sec.Headers, headers) || sec.Match.Path != "/v1/" {
		t.Errorf("the second set did not replace the first: %+v", sec)
	}

	value, err := s.Value("TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if value != "second-value-2" {
		t.Errorf("Value after the rotation %q", value)
	}

	if err := s.Remove("TOKEN"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := s.Remove("TOKEN"); err != nil {
		t.Errorf("a second Remove failed: %v", err)
	}
	if _, err := s.Get("TOKEN"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Remove = %v, want ErrNotFound", err)
	}
}

func TestSetWithOnlyAValueRotatesAndKeepsTheRest(t *testing.T) {
	s, _ := newStore(t)

	headers := []Header{{Name: "X-Api-Key", Value: "{value}"}}
	if _, err := s.Set("TOKEN", "first-value-1", []string{"a.example.com"}, headers, Match{Method: "POST"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set("TOKEN", "second-value-2", nil, nil, Match{}); err != nil {
		t.Fatalf("a rotation with no grant: %v", err)
	}

	sec, err := s.Get("TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sec.Destinations, ",") != "a.example.com" || !slices.Equal(sec.Headers, headers) || sec.Match.Method != "POST" {
		t.Errorf("the rotation changed the grant or the headers: %+v", sec)
	}

	value, err := s.Value("TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if value != "second-value-2" {
		t.Errorf("Value after the rotation %q", value)
	}
}

// writeOldRecord plants a record a release before this one wrote, with a placeholder of its own.
func writeOldRecord(t *testing.T, dir, name string) {
	t.Helper()

	blob, err := json.Marshal(record{Value: "old-value-1", Destinations: []string{"a.example.com"}, MockValue: "placeholder-token"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), blob, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSetRefusesToMoveAPlaceholderASandboxHolds(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")

	s, err := New(dir, func(string) ([]string, error) { return []string{"sandbox1"}, nil })
	if err != nil {
		t.Fatal(err)
	}
	writeOldRecord(t, dir, "TOKEN")

	_, err = s.Set("TOKEN", "new-value-2", nil, nil, Match{})
	if err == nil || !strings.Contains(err.Error(), "sandbox1") || !strings.Contains(err.Error(), "ungrant") {
		t.Errorf("a rotation over a held placeholder = %v, want a refusal naming sandbox1", err)
	}

	value, err := s.Value("TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if value != "old-value-1" {
		t.Errorf("the refused rotation wrote the value")
	}

	// With the holders gone the rotation lands, and the record moves to the one placeholder.
	s, err = New(dir, func(string) ([]string, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set("TOKEN", "new-value-2", nil, nil, Match{}); err != nil {
		t.Fatalf("a rotation once ungranted: %v", err)
	}
	rec, err := s.read("TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if rec.MockValue != "mock-TOKEN" {
		t.Errorf("the placeholder after the rotation is %q", rec.MockValue)
	}
}

func TestSetRefusesToMoveAPlaceholderItCannotAccountFor(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")

	s, err := New(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeOldRecord(t, dir, "TOKEN")

	if _, err := s.Set("TOKEN", "new-value-2", nil, nil, Match{}); err == nil || !strings.Contains(err.Error(), "ungrant") {
		t.Errorf("a rotation with no holders callback = %v, want a refusal", err)
	}

	s, err = New(dir, func(string) ([]string, error) { return nil, os.ErrPermission })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set("TOKEN", "new-value-2", nil, nil, Match{}); !errors.Is(err, os.ErrPermission) {
		t.Errorf("a rotation with unreadable holders = %v, want the read error", err)
	}
}

func TestListReturnsTheReadableSecretsWithTheError(t *testing.T) {
	s, dir := newStore(t)

	if _, err := s.Set("TOKEN", "some-value-123", []string{"example.com"}, nil, Match{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "BROKEN"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	list, err := s.List()
	if err == nil || !strings.Contains(err.Error(), "BROKEN") {
		t.Errorf("List with a broken file = %v, want an error naming BROKEN", err)
	}
	if len(list) != 1 || list[0].Name != "TOKEN" {
		t.Errorf("List = %+v, want TOKEN", list)
	}
	if err := s.Remove("BROKEN"); err != nil {
		t.Errorf("Remove of the broken file: %v", err)
	}
}

func TestListSkipsWhatIsNotASecret(t *testing.T) {
	s, dir := newStore(t)

	if _, err := s.Set("TOKEN", "some-value-123", []string{"example.com"}, nil, Match{}); err != nil {
		t.Fatal(err)
	}
	// What an interrupted atomic write leaves behind, and a stray file an operator dropped in.
	for _, name := range []string{".TOKEN.tmp123", "lowercase", "README.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("List = %+v, want only TOKEN", list)
	}
}

func TestSetRefusals(t *testing.T) {
	s, _ := newStore(t)

	cases := []struct {
		name    string
		key     string
		value   string
		to      []string
		headers []Header
		match   Match
		want    string
	}{
		{"lowercase name", "api_key", "v-1234567", []string{"example.com"}, nil, Match{}, "environment variable name"},
		{"digit first", "1KEY", "v-1234567", []string{"example.com"}, nil, Match{}, "environment variable name"},
		{"empty value", "KEY", "", []string{"example.com"}, nil, Match{}, "empty value"},
		{"no destination", "KEY", "v-1234567", nil, nil, Match{}, "no destination"},
		{"scheme in destination", "KEY", "v-1234567", []string{"https://example.com"}, nil, Match{}, "no scheme"},
		{"port in destination", "KEY", "v-1234567", []string{"example.com:443"}, nil, Match{}, "no scheme"},
		{"address destination", "KEY", "v-1234567", []string{"10.0.0.1"}, nil, Match{}, "is an address"},
		{"bare label", "KEY", "v-1234567", []string{"localhost"}, nil, Match{}, "has no dot"},
		{"bare wildcard", "KEY", "v-1234567", []string{"*"}, nil, Match{}, "has no dot"},
		{"bad label", "KEY", "v-1234567", []string{"exa_mple.com"}, nil, Match{}, "not a host name"},
		{"placeholder inside the value", "KEY", "abc-mock-KEY-1", []string{"example.com"}, nil, Match{}, "inside its value"},
		{"bad header name", "KEY", "v-1234567", []string{"example.com"}, []Header{{Name: "X Key", Value: "{value}"}}, Match{}, "not a header name"},
		{"empty header value", "KEY", "v-1234567", []string{"example.com"}, []Header{{Name: "X-Key"}}, Match{}, "no value"},
		{"line break in header", "KEY", "v-1234567", []string{"example.com"}, []Header{{Name: "X-Key", Value: "a\r\nb"}}, Match{}, "line break"},
		{"relative match path", "KEY", "v-1234567", []string{"example.com"}, nil, Match{Path: "v1"}, "start with /"},
		{"bare match query", "KEY", "v-1234567", []string{"example.com"}, nil, Match{Query: []string{"team"}}, "key=value"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Set(tc.key, tc.value, tc.to, tc.headers, tc.match)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Set = %v, want an error mentioning %q", err, tc.want)
			}
		})
	}

	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("a refused Set still wrote: %+v", list)
	}
}

func TestParseHeaderAndMatchReadTheFlagSpellings(t *testing.T) {
	header, err := ParseHeader("Authorization: Bearer {value}")
	if err != nil || header.Name != "Authorization" || header.Value != "Bearer {value}" {
		t.Errorf("ParseHeader = %+v, %v", header, err)
	}
	if _, err := ParseHeader("no-colon"); err == nil {
		t.Error("ParseHeader accepted a header with no colon")
	}

	match, err := ParseMatch([]string{"path=/v1/", "method=post", "query=team=blue", "header=X-Env=prod"})
	if err != nil || match.Path != "/v1/" || match.Method != "POST" || !slices.Equal(match.Query, []string{"team=blue"}) || !slices.Equal(match.Headers, []string{"X-Env=prod"}) {
		t.Errorf("ParseMatch = %+v, %v", match, err)
	}
	for _, bad := range []string{"host=example.com", "path", "query=team"} {
		if _, err := ParseMatch([]string{bad}); err == nil {
			t.Errorf("ParseMatch accepted %q", bad)
		}
	}
}

func TestReadRefusesANameThatEscapesTheStore(t *testing.T) {
	s, _ := newStore(t)

	for _, name := range []string{"../etc/passwd", "a/b", ".", ""} {
		if _, err := s.Get(name); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q) = %v, want a name refusal", name, err)
		}
		if err := s.Remove(name); err == nil {
			t.Errorf("Remove(%q) succeeded", name)
		}
	}
}
