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

	s, err := New(dir, func(string) ([]string, error) { return nil, nil })
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
	if sec.Placeholder != "mock-API_KEY" {
		t.Errorf("placeholder %q, want mock-API_KEY", sec.Placeholder)
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
	if _, err := s.Set("TOKEN", value, []string{"example.com"}, ""); err != nil {
		t.Fatalf("Set: %v", err)
	}

	sec, err := s.Get("TOKEN")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if strings.Contains(sec.Name+strings.Join(sec.Destinations, "")+sec.Placeholder, value) {
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
	if _, err := s.Set("TOKEN", "second-value-2", []string{"b.example.com"}, ""); err != nil {
		t.Fatal(err)
	}

	sec, err := s.Get("TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sec.Destinations, ",") != "b.example.com" {
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

	if _, err := s.Set("TOKEN", "first-value-1", []string{"a.example.com"}, "sk_test_shaped01"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set("TOKEN", "second-value-2", nil, ""); err != nil {
		t.Fatalf("a rotation with no grant: %v", err)
	}

	sec, err := s.Get("TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sec.Destinations, ",") != "a.example.com" {
		t.Errorf("the rotation changed the grant: %+v", sec)
	}
	if sec.Placeholder != "sk_test_shaped01" {
		t.Errorf("the rotation moved the placeholder to %q", sec.Placeholder)
	}

	value, err := s.Value("TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if value != "second-value-2" {
		t.Errorf("Value after the rotation %q", value)
	}
}

func TestSetRefusesToMoveAPlaceholderASandboxHolds(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")

	held, err := New(dir, func(string) ([]string, error) { return []string{"sandbox1"}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := held.Set("TOKEN", "old-value-1", []string{"a.example.com"}, "sk_test_first001"); err != nil {
		t.Fatal(err)
	}

	_, err = held.Set("TOKEN", "new-value-2", nil, "sk_test_second02")
	if err == nil || !strings.Contains(err.Error(), "sandbox1") || !strings.Contains(err.Error(), "ungrant") {
		t.Errorf("a change of a held placeholder = %v, want a refusal naming sandbox1", err)
	}

	value, err := held.Value("TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if value != "old-value-1" {
		t.Error("the refused change wrote the value")
	}

	// The value alone still rotates while a sandbox holds it: only the placeholder is what the guest holds.
	if _, err := held.Set("TOKEN", "new-value-2", nil, ""); err != nil {
		t.Errorf("a rotation that keeps the placeholder: %v", err)
	}

	// With the holders gone the change lands.
	free, err := New(dir, func(string) ([]string, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := free.Set("TOKEN", "new-value-3", nil, "sk_test_second02"); err != nil {
		t.Fatalf("a change once ungranted: %v", err)
	}
	sec, err := free.Get("TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if sec.Placeholder != "sk_test_second02" {
		t.Errorf("the placeholder after the change is %q", sec.Placeholder)
	}
}

func TestSetRefusesToMoveAPlaceholderItCannotAccountFor(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")

	seed, err := New(dir, func(string) ([]string, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Set("TOKEN", "old-value-1", []string{"a.example.com"}, "sk_test_first001"); err != nil {
		t.Fatal(err)
	}

	blind, err := New(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blind.Set("TOKEN", "new-value-2", nil, "sk_test_second02"); err == nil || !strings.Contains(err.Error(), "ungrant") {
		t.Errorf("a change with no holders callback = %v, want a refusal", err)
	}

	broken, err := New(dir, func(string) ([]string, error) { return nil, os.ErrPermission })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broken.Set("TOKEN", "new-value-2", nil, "sk_test_second02"); !errors.Is(err, os.ErrPermission) {
		t.Errorf("a change with unreadable holders = %v, want the read error", err)
	}
}

func TestSetRefusesAPlaceholderAnotherSecretOwns(t *testing.T) {
	s, _ := newStore(t)

	if _, err := s.Set("TOKEN", "first-value-1", []string{"a.example.com"}, "sk_test_taken001"); err != nil {
		t.Fatal(err)
	}

	for _, chosen := range []string{"sk_test_taken001", "mock-TOKEN"} {
		_, err := s.Set("OTHER", "second-value-2", []string{"b.example.com"}, chosen)
		if err == nil || !strings.Contains(err.Error(), "TOKEN") {
			t.Errorf("Set with the placeholder %q = %v, want a refusal naming TOKEN", chosen, err)
		}
	}

	// Its own placeholder is not another secret's, so a rotation that names it again lands.
	if _, err := s.Set("TOKEN", "third-value-3", nil, "sk_test_taken001"); err != nil {
		t.Errorf("a rotation that names the placeholder it already has: %v", err)
	}
}

func TestSetRefusesAPlaceholderItCannotTellApart(t *testing.T) {
	s, dir := newStore(t)

	if err := os.WriteFile(filepath.Join(dir, "BROKEN"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The default is not exempt: another secret may already have chosen it, and a broken file cannot say.
	for _, chosen := range []string{"sk_test_shaped01", ""} {
		_, err := s.Set("TOKEN", "some-value-123", []string{"a.example.com"}, chosen)
		if err == nil || !strings.Contains(err.Error(), "BROKEN") {
			t.Errorf("Set with the placeholder %q over an unreadable store = %v, want a refusal naming BROKEN", chosen, err)
		}
	}
}

// A default is a placeholder like any other: two secrets that share one substitute by list order.
func TestSetRefusesADefaultPlaceholderAnotherSecretChose(t *testing.T) {
	s, _ := newStore(t)

	if _, err := s.Set("FOO", "first-value-1", []string{"a.example.com"}, "mock-BAR"); err != nil {
		t.Fatalf("a placeholder no stored secret owns: %v", err)
	}

	_, err := s.Set("BAR", "second-value-2", []string{"b.example.com"}, "")
	if err == nil || !strings.Contains(err.Error(), "FOO") {
		t.Errorf("Set with the default mock-BAR = %v, want a refusal naming FOO", err)
	}
}

func TestTheDefaultPlaceholderIsExemptFromTheLengthRule(t *testing.T) {
	s, _ := newStore(t)

	sec, err := s.Set("K", "some-value-123", []string{"a.example.com"}, "")
	if err != nil {
		t.Fatalf("Set of a one-letter name: %v", err)
	}
	if sec.Placeholder != "mock-K" || len(sec.Placeholder) >= minPlaceholder {
		t.Errorf("the default placeholder is %q, want a short one the rule does not reach", sec.Placeholder)
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
		name        string
		key         string
		value       string
		to          []string
		placeholder string
		want        string
	}{
		{"lowercase name", "api_key", "v-1234567", []string{"example.com"}, "", "environment variable name"},
		{"digit first", "1KEY", "v-1234567", []string{"example.com"}, "", "environment variable name"},
		{"empty value", "KEY", "", []string{"example.com"}, "", "empty value"},
		{"no destination", "KEY", "v-1234567", nil, "", "no destination"},
		{"scheme in destination", "KEY", "v-1234567", []string{"https://example.com"}, "", "no scheme"},
		{"port in destination", "KEY", "v-1234567", []string{"example.com:443"}, "", "no scheme"},
		{"address destination", "KEY", "v-1234567", []string{"10.0.0.1"}, "", "is an address"},
		{"bare label", "KEY", "v-1234567", []string{"localhost"}, "", "has no dot"},
		{"bare wildcard", "KEY", "v-1234567", []string{"*"}, "", "has no dot"},
		{"bad label", "KEY", "v-1234567", []string{"exa_mple.com"}, "", "not a host name"},
		{"default placeholder inside the value", "KEY", "abc-mock-KEY-1", []string{"example.com"}, "", "inside its value"},
		{"chosen placeholder inside the value", "KEY", "abc-sk_test_shaped01-1", []string{"example.com"}, "sk_test_shaped01", "inside its value"},
		{"placeholder too short", "KEY", "v-1234567", []string{"example.com"}, "sk_test", "shorter than"},
		{"whitespace in the placeholder", "KEY", "v-1234567", []string{"example.com"}, "sk test shaped", "outside letters, digits"},
		{"control character in the placeholder", "KEY", "v-1234567", []string{"example.com"}, "sk_test\x01shaped", "outside letters, digits"},
		{"a plus in the placeholder", "KEY", "v-1234567", []string{"example.com"}, "sk_test+shaped", "outside letters, digits"},
		{"a slash in the placeholder", "KEY", "v-1234567", []string{"example.com"}, "sk_test/shaped", "outside letters, digits"},
		{"a quote in the placeholder", "KEY", "v-1234567", []string{"example.com"}, `sk_test"shaped`, "outside letters, digits"},
		{"an equals in the placeholder", "KEY", "v-1234567", []string{"example.com"}, "sk_test=shaped", "outside letters, digits"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Set(tc.key, tc.value, tc.to, tc.placeholder)
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

// A placeholder outside the set is rewritten by the client's encoder, so the proxy would never find it.
func TestSetTakesAPlaceholderNoEncoderAlters(t *testing.T) {
	s, _ := newStore(t)

	sec, err := s.Set("KEY", "v-1234567", []string{"example.com"}, "sk_test.e2e-01")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if sec.Placeholder != "sk_test.e2e-01" {
		t.Errorf("the store kept %q", sec.Placeholder)
	}
}

// A mistyped --placeholder hands the value as the placeholder, so no refusal may put it on a screen.
func TestARefusedPlaceholderNeverEchoesIt(t *testing.T) {
	s, _ := newStore(t)

	for _, chosen := range []string{"sk+live+abcdef123456", "sk/live/abcdef123456", `sk"live"abcdef123456`, "sk=live=abcdef123456", "sk live abcdef123456"} {
		_, err := s.Set("KEY", "v-1234567", []string{"example.com"}, chosen)
		if err == nil {
			t.Fatalf("Set took the placeholder %q", chosen)
		}
		if strings.Contains(err.Error(), chosen) {
			t.Errorf("the refusal echoes the placeholder it refused: %v", err)
		}
	}
}

// The default is exempt from the shape rules, and a name that could fail the set cannot exist anyway.
func TestTheDefaultPlaceholderAlwaysFitsTheSet(t *testing.T) {
	if !placeholderCharset.MatchString(DefaultPlaceholder("A")) {
		t.Errorf("the default placeholder of the shortest legal name is outside the set")
	}
}

func TestValidNameNeverEchoesTheNameItRefused(t *testing.T) {
	s, _ := newStore(t)

	// A mistyped set hands the value as the name, so the refusal must not put it on a screen or in a log.
	for _, name := range []string{"sk-live-abcdef123456", strings.Repeat("A", maxChars+1)} {
		err := ValidName(name)
		if err == nil {
			t.Fatalf("ValidName(%d characters) took the name", len(name))
		}
		if strings.Contains(err.Error(), name) {
			t.Errorf("the refusal echoes the name it refused")
		}

		_, err = s.Set(name, "some-value-123", []string{"api.example.com"}, "")
		if err == nil {
			t.Fatal("Set took the name")
		}
		if strings.Contains(err.Error(), name) {
			t.Errorf("the Set refusal echoes the name it refused")
		}
	}
}

func TestValidDestinationNeverEchoesTheDestinationItRefused(t *testing.T) {
	s, _ := newStore(t)

	// A mistyped --to hands the value as the destination, and every refusal keeps it off the screen.
	for _, dest := range []string{"sk-live-abcdef123456", "https://sk-live-abcdef123456/v1", "10.0.0.1", "nodot", "api*.example.com", "sk_live_underscore.example.com", strings.Repeat("a", 254) + ".example.com"} {
		_, err := ValidDestination(dest)
		if err == nil {
			t.Fatalf("ValidDestination(%d characters) took the destination", len(dest))
		}
		if strings.Contains(err.Error(), dest) {
			t.Errorf("the refusal echoes the destination it refused: %v", err)
		}

		_, err = s.Set("TOKEN", "some-value-123", []string{dest}, "")
		if err == nil {
			t.Fatal("Set took the destination")
		}
		if strings.Contains(err.Error(), dest) {
			t.Errorf("the Set refusal echoes the destination it refused: %v", err)
		}
	}
}

func TestSetNamesThePositionOfTheDestinationItRefused(t *testing.T) {
	s, _ := newStore(t)

	_, err := s.Set("TOKEN", "some-value-123", []string{"api.example.com", "cdn.example.com", "bad_host", "ok.example.com"}, "")
	if err == nil {
		t.Fatal("Set took a bad destination")
	}
	if !strings.Contains(err.Error(), "3rd destination") {
		t.Errorf("Set = %v, want the position of the destination it refused", err)
	}
	if strings.Contains(err.Error(), "bad_host") {
		t.Errorf("the refusal echoes the destination it refused: %v", err)
	}
}
