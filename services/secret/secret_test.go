package secret

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "secrets")

	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return s, dir
}

func TestSetWritesOneFileNobodyElseCanRead(t *testing.T) {
	s, dir := newStore(t)

	sec, err := s.Set("API_KEY", "sk-live-1234567890", []string{"API.Example.com.", "api.example.com"}, "")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}

	if got, want := sec.Destinations, []string{"api.example.com"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("destinations %v, want the one canonical host %v", got, want)
	}
	if sec.MockValue != "mock-API_KEY" {
		t.Errorf("placeholder %q, want mock-API_KEY", sec.MockValue)
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

	if _, err := New(dir); err != nil {
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
	if _, err := s.Set("TOKEN", value, []string{"example.com"}, ""); err != nil {
		t.Fatalf("Set: %v", err)
	}

	sec, err := s.Get("TOKEN")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if strings.Contains(sec.Name+sec.MockValue+strings.Join(sec.Destinations, ""), value) {
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

	if _, err := s.Set("TOKEN", "first-value-1", []string{"a.example.com"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set("TOKEN", "second-value-2", []string{"b.example.com"}, "placeholder-token"); err != nil {
		t.Fatal(err)
	}

	sec, err := s.Get("TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sec.Destinations, ",") != "b.example.com" || sec.MockValue != "placeholder-token" {
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

	if _, err := s.Set("TOKEN", "first-value-1", []string{"a.example.com"}, "placeholder-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set("TOKEN", "second-value-2", nil, ""); err != nil {
		t.Fatalf("a rotation with no grant: %v", err)
	}

	sec, err := s.Get("TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sec.Destinations, ",") != "a.example.com" || sec.MockValue != "placeholder-token" {
		t.Errorf("the rotation changed the grant or the placeholder: %+v", sec)
	}

	value, err := s.Value("TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if value != "second-value-2" {
		t.Errorf("Value after the rotation %q", value)
	}
}

func TestSetGivesAShortNameItsDefaultPlaceholder(t *testing.T) {
	s, _ := newStore(t)

	sec, err := s.Set("K", "some-value-123", []string{"example.com"}, "")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if sec.MockValue != "mock-K" {
		t.Errorf("MockValue = %q, want mock-K", sec.MockValue)
	}
}

func TestListReturnsTheReadableSecretsWithTheError(t *testing.T) {
	s, dir := newStore(t)

	if _, err := s.Set("TOKEN", "some-value-123", []string{"example.com"}, ""); err != nil {
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

	if _, err := s.Set("TOKEN", "some-value-123", []string{"example.com"}, ""); err != nil {
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
		name  string
		key   string
		value string
		to    []string
		mock  string
		want  string
	}{
		{"lowercase name", "api_key", "v-1234567", []string{"example.com"}, "", "environment variable name"},
		{"digit first", "1KEY", "v-1234567", []string{"example.com"}, "", "environment variable name"},
		{"empty value", "KEY", "", []string{"example.com"}, "", "empty value"},
		{"no destination", "KEY", "v-1234567", nil, "", "no destination"},
		{"scheme in destination", "KEY", "v-1234567", []string{"https://example.com"}, "", "no scheme"},
		{"port in destination", "KEY", "v-1234567", []string{"example.com:443"}, "", "no scheme"},
		{"address destination", "KEY", "v-1234567", []string{"10.0.0.1"}, "", "is an address"},
		{"bare label", "KEY", "v-1234567", []string{"localhost"}, "", "has no dot"},
		{"bad label", "KEY", "v-1234567", []string{"exa_mple.com"}, "", "not a host name"},
		{"mock is the value", "KEY", "v-1234567", []string{"example.com"}, "v-1234567", "inside its value"},
		{"mock inside the value", "KEY", "abc-v-1234567", []string{"example.com"}, "v-1234567", "inside its value"},
		{"short mock", "KEY", "v-1234567", []string{"example.com"}, "abc", "shorter than"},
		{"mock with space", "KEY", "v-1234567", []string{"example.com"}, "mock value", "whitespace"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Set(tc.key, tc.value, tc.to, tc.mock)
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
