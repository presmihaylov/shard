package setup

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/presmihaylov/shard/services/client"
)

// testKey is a synthetic key no real credential holds, so any line that carries it is a leak.
const testKey = "shard657-synthetic-key-9e8d7c6b"

// someCaps leaves fork and snapshot unsupported, so a test sees both words.
const someCaps = `{"create":true,"start":true,"stop":true,"remove":true,"pause":true,"resume":true,"fork":false,"snapshot":false}`

// front is a shard serve that takes key alone; fail answers a request with a 503 while it returns true.
func front(key string, fail func() bool) *httptest.Server {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, status := `{"version":"test"}`, http.StatusOK
		switch {
		case r.Header.Get("Authorization") != "Bearer "+key:
			body, status = `{"error":{"code":"unauthorized","message":"the bearer token is missing or invalid"}}`, http.StatusUnauthorized
		case fail != nil && fail():
			body, status = `{"error":{"code":"unavailable","message":"the daemon is restarting"}}`, http.StatusServiceUnavailable
		case r.URL.Path == "/v0/capabilities":
			body = someCaps
		}
		w.WriteHeader(status)
		if _, err := w.Write([]byte(body)); err != nil {
			panic(fmt.Sprintf("write the response: %v", err))
		}
	}))
	// The untrusted certificate test fails a handshake on purpose.
	server.Config.ErrorLog = log.New(io.Discard, "", 0)

	return server
}

// tlsFront is front over https, with the CA file that trusts it.
func tlsFront(t *testing.T, key string, fail func() bool) (string, string) {
	t.Helper()

	server := front(key, fail)
	server.StartTLS()
	t.Cleanup(server.Close)

	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatalf("write the ca file: %v", err)
	}

	return server.URL, ca
}

// testHost is a machine whose environment is vars alone, with its configuration directory in a temp dir.
func testHost(t *testing.T, vars map[string]string) (Host, string) {
	t.Helper()

	env := map[string]string{client.ConfigHomeEnv: t.TempDir()}
	maps.Copy(env, vars)
	host := Host{Root: t.TempDir(), Env: func(name string) string { return env[name] }}
	path, err := client.ConfigPath(host.Env)
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}

	return host, path
}

// saveConnection writes c where the wizard finds it, as an earlier run would have.
func saveConnection(t *testing.T, path string, c client.Config) {
	t.Helper()

	if err := client.SaveConfig(path, c); err != nil {
		t.Fatalf("save the connection: %v", err)
	}
}

// savedConnection is what the file at path holds now, the zero Config for no file.
func savedConnection(t *testing.T, path string) client.Config {
	t.Helper()

	c, err := client.LoadConfig(path)
	if err != nil {
		t.Fatalf("load the connection: %v", err)
	}

	return c
}

// noLeak fails a test whose output or error carries the key.
func noLeak(t *testing.T, ui *fakeUI, err error) {
	t.Helper()

	for _, line := range ui.printed {
		if strings.Contains(line, testKey) {
			t.Errorf("printed the key: %q", line)
		}
	}
	for _, list := range ui.lists {
		for _, mark := range list.marks {
			if strings.Contains(mark, testKey) {
				t.Errorf("the checklist shows the key: %q", mark)
			}
		}
	}
	if err != nil && strings.Contains(err.Error(), testKey) {
		t.Errorf("the error quotes the key: %v", err)
	}
}

// names are the Name of each option a question offered, in order.
func names(ui *fakeUI, q Question) []string {
	var out []string
	for _, o := range ui.options[q] {
		out = append(out, o.Name)
	}

	return out
}

func containsAll(t *testing.T, printed []string, want ...string) {
	t.Helper()

	for _, line := range want {
		if !slices.Contains(printed, line) {
			t.Errorf("printed no line %q in %q", line, printed)
		}
	}
}

var allDone = []string{"start 0", "done 0", "start 1", "done 1", "start 2", "done 2"}

// The §13 completion text, quoted from the spec, after the path line.
var completion = []string{
	"The file stores your API key as plain text and is accessible only to your user.",
	"Shard will use this connection automatically.",
	"",
	"Next steps:",
	"",
	"  List sandboxes:",
	"    shard list",
	"",
	"  Create a sandbox:",
	"    shard create --name demo --memory 512MiB alpine:3.20",
	"",
	"  Run a command:",
	"    shard exec demo echo hello",
	"",
	"  Remove the sandbox:",
	"    shard remove --force demo",
	"",
	"Documentation: https://useshards.com/docs",
}

// A connection is checked in three steps, shows every capability, and is saved 0600 on a yes. (SHARD-657)
func TestRemoteVerifiesThenSavesTheConnection(t *testing.T) {
	url, ca := tlsFront(t, testKey, nil)
	host, path := testHost(t, map[string]string{client.CAFileEnv: ca})
	ui := &fakeUI{
		selects:  map[Question]string{AskMode: "remote"},
		texts:    map[Question]string{AskURL: url},
		secrets:  map[Question]string{AskAPIKey: testKey},
		confirms: map[Question]bool{AskSave: true},
	}

	err := (&Setup{Host: host, UI: ui}).Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if want := []Question{AskMode, AskURL, AskAPIKey, AskSave}; !slices.Equal(ui.asked, want) {
		t.Errorf("asked %v, want %v", ui.asked, want)
	}
	if len(ui.lists) != 1 || ui.lists[0].title != "Checking the connection" || !slices.Equal(ui.lists[0].steps, verifySteps) {
		t.Fatalf("drew %d checklists, want the one of %v", len(ui.lists), verifySteps)
	}
	if !slices.Equal(ui.lists[0].marks, allDone) {
		t.Errorf("marked %v, want %v", ui.lists[0].marks, allDone)
	}
	if got := savedConnection(t, path); got != (client.Config{Remote: url, APIKey: testKey}) {
		t.Errorf("saved %v, want the verified connection", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the configuration: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the configuration has mode %o, want 600", info.Mode().Perm())
	}

	containsAll(t, ui.printed,
		"Server capabilities:",
		"  create     supported", "  resume     supported", "  fork       not supported", "  snapshot   not supported",
		"Capabilities show what the server supports. They do not override the permissions of your API key.",
		"✓ Connection saved", "Configuration: "+path,
		"Keep "+client.CAFileEnv+" set: the saved connection does not store the certificate authority.",
	)
	// The checklist marks each step once, so the text after it repeats none of them. (SHARD-672)
	if checks := slices.DeleteFunc(slices.Clone(ui.printed), func(line string) bool { return !strings.HasPrefix(line, "✓ ") }); !slices.Equal(checks, []string{"✓ Connection saved"}) {
		t.Errorf("printed the check lines %q, want the save alone", checks)
	}
	for _, block := range [][]string{completion[:2], completion[2:]} {
		if !strings.Contains(strings.Join(ui.printed, "\n"), strings.Join(block, "\n")) {
			t.Errorf("printed %q, want the §13 completion text %q", ui.printed, block)
		}
	}
	noLeak(t, ui, err)
}

// HTTP warns and asks first; a no stops before anything is sent, and a yes connects. (SHARD-657)
func TestAnHTTPServerAsksFirst(t *testing.T) {
	server := front(testKey, nil)
	server.Start()
	t.Cleanup(server.Close)
	host, path := testHost(t, nil)

	ui := &fakeUI{texts: map[Question]string{AskURL: server.URL}, confirms: map[Question]bool{AskHTTP: false}}
	err := (&Setup{Host: host, UI: ui}).remote(t.Context())
	if !errors.Is(err, ErrDeclined) {
		t.Fatalf("remote after a no: %v, want ErrDeclined", err)
	}
	if want := []Question{AskURL, AskHTTP}; !slices.Equal(ui.asked, want) {
		t.Errorf("asked %v, want %v", ui.asked, want)
	}
	containsAll(t, ui.printed, "HTTP does not encrypt your API key or requests.", "Use it only through a trusted encrypted network.")
	if len(ui.lists) != 0 {
		t.Errorf("checked the connection after a no")
	}

	ui = &fakeUI{
		texts:    map[Question]string{AskURL: server.URL},
		secrets:  map[Question]string{AskAPIKey: testKey},
		confirms: map[Question]bool{AskHTTP: true, AskSave: true},
	}
	if err := (&Setup{Host: host, UI: ui}).remote(t.Context()); err != nil {
		t.Fatalf("remote after a yes: %v", err)
	}
	if got := savedConnection(t, path); got.Remote != server.URL {
		t.Errorf("saved %v, want %s", got, server.URL)
	}
}

// SHARD_API_KEY answers the key, so a person or a script is not asked for it again. (SHARD-657)
func TestSHARDAPIKEYIsUsedRatherThanAsked(t *testing.T) {
	url, ca := tlsFront(t, testKey, nil)
	host, path := testHost(t, map[string]string{client.CAFileEnv: ca, client.APIKeyEnv: testKey})
	ui := &fakeUI{texts: map[Question]string{AskURL: url}, confirms: map[Question]bool{AskSave: true}}

	err := (&Setup{Host: host, UI: ui}).remote(t.Context())
	if err != nil {
		t.Fatalf("remote: %v", err)
	}

	if slices.Contains(ui.asked, AskAPIKey) {
		t.Errorf("asked for the key with %s set", client.APIKeyEnv)
	}
	containsAll(t, ui.printed, "Using the API key in "+client.APIKeyEnv+".")
	if got := savedConnection(t, path); got.APIKey != testKey {
		t.Errorf("saved a different key than %s", client.APIKeyEnv)
	}
	noLeak(t, ui, err)
}

// A refused key fails the authentication step, offers retry, edit or exit, and on exit stops with nothing saved. (SHARD-657)
func TestARefusedKeyOffersRetryEditOrExit(t *testing.T) {
	url, ca := tlsFront(t, "the-accepted-key", nil)
	host, path := testHost(t, map[string]string{client.CAFileEnv: ca})
	ui := &fakeUI{
		texts:   map[Question]string{AskURL: url},
		secrets: map[Question]string{AskAPIKey: testKey},
		selects: map[Question]string{AskRetry: "exit"},
	}

	err := (&Setup{Host: host, UI: ui}).remote(t.Context())

	var refused *client.APIError
	if !errors.As(err, &refused) || refused.Status != http.StatusUnauthorized {
		t.Fatalf("remote returned %v, want the 401", err)
	}
	var stopped *StoppedError
	if !errors.As(err, &stopped) || stopped.Step != "Verify authentication" {
		t.Errorf("remote returned %v, want it stopped at the authentication step", err)
	}
	want := []string{"start 0", "done 0", "start 1", "fail 1: The server did not accept the API key. / Check the key, or ask the server administrator for a new one."}
	if !slices.Equal(ui.lists[0].marks, want) {
		t.Errorf("marked %v, want %v", ui.lists[0].marks, want)
	}
	if got := names(ui, AskRetry); !slices.Equal(got, []string{"retry", "edit", "exit"}) {
		t.Errorf("offered %v after a failure, want retry, edit and exit", got)
	}
	if got := savedConnection(t, path); got != (client.Config{}) {
		t.Errorf("saved %v after a failed check", got)
	}
	if tail := ui.printed[len(ui.printed)-2:]; !slices.Equal(tail, []string{"", "Nothing was saved."}) {
		t.Errorf("Exit printed %q last, want that nothing was saved", tail)
	}
	noLeak(t, ui, err)
}

// keysUI answers each key prompt from keys in turn, so a test can fix a wrong key through Edit.
type keysUI struct {
	*fakeUI
	keys    []string
	prompts []string
}

func (u *keysUI) Secret(_ context.Context, q Question, prompt string) (string, error) {
	u.ask(q)
	u.prompts = append(u.prompts, prompt)
	if len(u.keys) == 0 {
		return "", fmt.Errorf("secret %s: %w", q, errUnscripted)
	}
	key := u.keys[0]
	u.keys = u.keys[1:]

	return key, nil
}

// Edit asks for the details again and checks them from the first step. (SHARD-657)
func TestEditAsksAgainAndChecksTheNewDetails(t *testing.T) {
	url, ca := tlsFront(t, testKey, nil)
	host, path := testHost(t, map[string]string{client.CAFileEnv: ca})
	fake := &fakeUI{
		texts:    map[Question]string{AskURL: url},
		selects:  map[Question]string{AskRetry: "edit"},
		confirms: map[Question]bool{AskSave: true},
	}
	ui := &keysUI{fakeUI: fake, keys: []string{"a-wrong-key", testKey}}

	if err := (&Setup{Host: host, UI: ui}).remote(t.Context()); err != nil {
		t.Fatalf("remote: %v", err)
	}

	if want := []Question{AskURL, AskAPIKey, AskRetry, AskURL, AskAPIKey, AskSave}; !slices.Equal(fake.asked, want) {
		t.Errorf("asked %v, want %v", fake.asked, want)
	}
	if want := []string{"", url}; !slices.Equal(fake.initials, want) {
		t.Errorf("the URL started as %q, want empty and then the URL to edit (SHARD-667)", fake.initials)
	}
	if len(fake.lists) != 2 || !slices.Equal(fake.lists[1].marks, allDone) {
		t.Fatalf("drew %d checklists, want a second that passes", len(fake.lists))
	}
	if got := savedConnection(t, path); got.APIKey != testKey {
		t.Errorf("saved a key other than the edited one")
	}
}

// An edit asks for the key even with SHARD_API_KEY set, since that key is the one the server refused. (SHARD-657)
func TestEditAsksForTheKeyThatSHARDAPIKEYGotWrong(t *testing.T) {
	url, ca := tlsFront(t, testKey, nil)
	host, path := testHost(t, map[string]string{client.CAFileEnv: ca, client.APIKeyEnv: "a-stale-key"})
	ui := &fakeUI{
		texts:    map[Question]string{AskURL: url},
		secrets:  map[Question]string{AskAPIKey: testKey},
		selects:  map[Question]string{AskRetry: "edit"},
		confirms: map[Question]bool{AskSave: true},
	}

	err := (&Setup{Host: host, UI: ui}).remote(t.Context())
	if err != nil {
		t.Fatalf("remote: %v", err)
	}

	if want := []Question{AskURL, AskRetry, AskURL, AskAPIKey, AskSave}; !slices.Equal(ui.asked, want) {
		t.Errorf("asked %v, want %v", ui.asked, want)
	}
	if got := savedConnection(t, path); got.APIKey != testKey {
		t.Errorf("saved a key other than the edited one")
	}
	containsAll(t, ui.printed, client.APIKeyEnv+" is set to another key, and Shard commands use it before the saved one.")
	noLeak(t, ui, err)
}

// Retry checks the same details again, without asking for them. (SHARD-657)
func TestRetryChecksTheSameDetailsAgain(t *testing.T) {
	var requests atomic.Int32
	url, ca := tlsFront(t, testKey, func() bool { return requests.Add(1) == 1 })
	host, path := testHost(t, map[string]string{client.CAFileEnv: ca})
	ui := &fakeUI{
		texts:    map[Question]string{AskURL: url},
		secrets:  map[Question]string{AskAPIKey: testKey},
		selects:  map[Question]string{AskRetry: "retry"},
		confirms: map[Question]bool{AskSave: true},
	}

	if err := (&Setup{Host: host, UI: ui}).remote(t.Context()); err != nil {
		t.Fatalf("remote: %v", err)
	}

	if want := []Question{AskURL, AskAPIKey, AskRetry, AskSave}; !slices.Equal(ui.asked, want) {
		t.Errorf("asked %v, want %v", ui.asked, want)
	}
	if len(ui.lists) != 2 || !strings.HasPrefix(ui.lists[0].marks[3], "fail 1: the daemon is restarting") || !slices.Equal(ui.lists[1].marks, allDone) {
		t.Errorf("drew %d checklists, want a 503 then a pass", len(ui.lists))
	}
	if got := savedConnection(t, path); got.Remote != url {
		t.Errorf("saved %v after the retry", got)
	}
}

// An untrusted certificate fails the first step, explains SHARD_CA_FILE, and offers no way around the check. (SHARD-657)
func TestAnUntrustedCertificateExplainsSHARDCAFILE(t *testing.T) {
	url, _ := tlsFront(t, testKey, nil)
	host, _ := testHost(t, nil)
	ui := &fakeUI{
		texts:   map[Question]string{AskURL: url},
		secrets: map[Question]string{AskAPIKey: testKey},
		selects: map[Question]string{AskRetry: "exit"},
	}

	err := (&Setup{Host: host, UI: ui}).remote(t.Context())

	var untrusted *tls.CertificateVerificationError
	var stopped *StoppedError
	if !errors.As(err, &untrusted) || !errors.As(err, &stopped) || stopped.Step != "Reach the server" {
		t.Fatalf("remote returned %v, want the certificate error, stopped at the first step", err)
	}
	want := []string{"start 0", "fail 0: This machine does not trust the server certificate. / For a private certificate authority, exit, set SHARD_CA_FILE to its PEM file, and run shard setup again."}
	if !slices.Equal(ui.lists[0].marks, want) {
		t.Errorf("marked %v, want %v", ui.lists[0].marks, want)
	}
	if got := names(ui, AskRetry); !slices.Equal(got, []string{"retry", "edit", "exit"}) {
		t.Errorf("offered %v, want no choice beyond retry, edit and exit", got)
	}
	noLeak(t, ui, err)
}

// A server nothing answers on fails the first step in the §10 shape: what failed, why, and what to check. (SHARD-657)
func TestAnUnreachableServerSaysWhyAndWhatToCheck(t *testing.T) {
	server := front(testKey, nil)
	server.StartTLS()
	url := server.URL
	server.Close()
	host, _ := testHost(t, nil)
	ui := &fakeUI{
		texts:   map[Question]string{AskURL: url},
		secrets: map[Question]string{AskAPIKey: testKey},
		selects: map[Question]string{AskRetry: "exit"},
	}

	err := (&Setup{Host: host, UI: ui}).remote(t.Context())

	var stopped *StoppedError
	if !errors.As(err, &stopped) || stopped.Step != "Reach the server" {
		t.Fatalf("remote returned %v, want it stopped at the first step", err)
	}
	want := []string{"start 0", "fail 0: Could not reach " + url + ": connection refused. / Check the URL, and that shard serve or the proxy in front of it runs."}
	if !slices.Equal(ui.lists[0].marks, want) {
		t.Errorf("marked %v, want %v", ui.lists[0].marks, want)
	}
	if tail := ui.printed[len(ui.printed)-2:]; !slices.Equal(tail, []string{"", "Nothing was saved."}) {
		t.Errorf("Exit printed %q last, want that nothing was saved (SHARD-667)", tail)
	}
}

// Enter at the key prompt of an edit keeps the key, which the prompt never shows. (SHARD-667)
func TestEditKeepsTheKeyOnAnEmptyAnswer(t *testing.T) {
	var requests atomic.Int32
	url, ca := tlsFront(t, testKey, func() bool { return requests.Add(1) == 1 })
	host, path := testHost(t, map[string]string{client.CAFileEnv: ca})
	fake := &fakeUI{
		texts:    map[Question]string{AskURL: url},
		selects:  map[Question]string{AskRetry: "edit"},
		confirms: map[Question]bool{AskSave: true},
	}
	ui := &keysUI{fakeUI: fake, keys: []string{testKey, ""}}

	err := (&Setup{Host: host, UI: ui}).remote(t.Context())
	if err != nil {
		t.Fatalf("remote: %v", err)
	}

	if want := []string{"API key:", "API key (press Enter to keep the current key):"}; !slices.Equal(ui.prompts, want) {
		t.Errorf("the key prompts were %q, want %q", ui.prompts, want)
	}
	if got := savedConnection(t, path); got.APIKey != testKey {
		t.Errorf("saved a key other than the kept one")
	}
	noLeak(t, fake, err)
}

// Exit after any failed check of a saved connection says it stays as it was. (SHARD-667)
func TestExitAfterAFailedCheckLeavesTheSavedConnection(t *testing.T) {
	refused, ca := tlsFront(t, "the-accepted-key", nil)
	closed := front(testKey, nil)
	closed.StartTLS()
	closed.Close()
	for name, url := range map[string]string{"a refused key": refused, "an unreachable server": closed.URL} {
		t.Run(name, func(t *testing.T) {
			host, path := testHost(t, map[string]string{client.CAFileEnv: ca})
			saved := client.Config{Remote: url, APIKey: testKey}
			saveConnection(t, path, saved)
			ui := &fakeUI{selects: map[Question]string{AskMode: "remote", AskSaved: "check", AskRetry: "exit"}}

			err := (&Setup{Host: host, UI: ui}).Run(t.Context())
			if _, ok := errors.AsType[*StoppedError](err); !ok {
				t.Fatalf("Run returned %v, want a stopped check", err)
			}
			if tail := ui.printed[len(ui.printed)-2:]; !slices.Equal(tail, []string{"", "The saved connection is unchanged."}) {
				t.Errorf("Exit printed %q last, want that the saved connection is unchanged", tail)
			}
			if got := savedConnection(t, path); got != saved {
				t.Errorf("the failed check changed the saved connection to %v", got)
			}
			noLeak(t, ui, err)
		})
	}
}

// A declined save writes nothing and says how to use the connection, with the key as a placeholder. (SHARD-657)
func TestADeclinedSaveWritesNothing(t *testing.T) {
	url, ca := tlsFront(t, testKey, nil)
	host, path := testHost(t, map[string]string{client.CAFileEnv: ca})
	ui := &fakeUI{
		texts:    map[Question]string{AskURL: url},
		secrets:  map[Question]string{AskAPIKey: testKey},
		confirms: map[Question]bool{AskSave: false},
	}

	err := (&Setup{Host: host, UI: ui}).remote(t.Context())
	if err != nil {
		t.Fatalf("remote: %v", err)
	}

	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat %s returned %v after a declined save, want no file", path, err)
	}
	containsAll(t, ui.printed,
		"The connection was verified but not saved.",
		"  export "+client.RemoteEnv+"="+url,
		"  export "+client.APIKeyEnv+"=<your API key>",
		"  export "+client.CAFileEnv+"="+ca,
	)
	// The plain-text note is about the file, so a run that writes none never shows it. (SHARD-672)
	if slices.ContainsFunc(ui.printed, func(line string) bool { return strings.Contains(line, "plain text") }) {
		t.Errorf("printed %q, want no plain-text note without a save", ui.printed)
	}
	noLeak(t, ui, err)
}

// A saved connection opens the §14 menu, and Check verifies it as saved without asking to save it again. (SHARD-657)
func TestASavedConnectionCanBeChecked(t *testing.T) {
	url, ca := tlsFront(t, testKey, nil)
	host, path := testHost(t, map[string]string{client.CAFileEnv: ca})
	saved := client.Config{Remote: url, APIKey: testKey}
	saveConnection(t, path, saved)
	ui := &fakeUI{selects: map[Question]string{AskMode: "remote", AskSaved: "check"}}

	err := (&Setup{Host: host, UI: ui}).Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if ui.printed[0] != "Saved connection: "+url {
		t.Errorf("printed %q first, want the saved connection", ui.printed[0])
	}
	if got := names(ui, AskSaved); !slices.Equal(got, []string{"check", "replace", "remove", "exit"}) {
		t.Errorf("the menu offers %v", got)
	}
	if want := []Question{AskMode, AskSaved}; !slices.Equal(ui.asked, want) {
		t.Errorf("asked %v, want %v", ui.asked, want)
	}
	if len(ui.lists) != 1 || !slices.Equal(ui.lists[0].marks, allDone) {
		t.Errorf("drew %d checklists, want one that passes", len(ui.lists))
	}
	containsAll(t, ui.printed, "Server capabilities:", "  snapshot   not supported")
	if got := savedConnection(t, path); got != saved {
		t.Errorf("a check changed the saved connection to %v", got)
	}
	noLeak(t, ui, err)
}

// Replace verifies the new connection before it is saved, and a failure or a no keeps the old one. (SHARD-657)
func TestReplaceKeepsTheOldConnectionUntilTheNewOneIsSaved(t *testing.T) {
	oldURL, _ := tlsFront(t, "old-key", nil)
	newURL, ca := tlsFront(t, testKey, nil)
	host, path := testHost(t, map[string]string{client.CAFileEnv: ca})
	old := client.Config{Remote: oldURL, APIKey: "old-key"}
	saveConnection(t, path, old)

	for _, tc := range []struct {
		name string
		key  string
		save bool
		want client.Config
	}{
		{name: "failed check", key: "a-wrong-key", want: old},
		{name: "declined save", key: testKey, want: old},
		{name: "saved", key: testKey, save: true, want: client.Config{Remote: newURL, APIKey: testKey}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ui := &fakeUI{
				selects:  map[Question]string{AskSaved: "replace", AskRetry: "exit"},
				texts:    map[Question]string{AskURL: newURL},
				secrets:  map[Question]string{AskAPIKey: tc.key},
				confirms: map[Question]bool{AskSave: tc.save},
			}
			err := (&Setup{Host: host, UI: ui}).remote(t.Context())
			if tc.key == testKey && err != nil {
				t.Fatalf("remote: %v", err)
			}
			if got := savedConnection(t, path); got != tc.want {
				t.Errorf("the saved connection is %v, want %v", got, tc.want)
			}
			if tc.key == testKey && !tc.save {
				containsAll(t, ui.printed, "The saved connection to "+oldURL+" is unchanged.")
			}
			noLeak(t, ui, err)
		})
	}
}

// Remove deletes the saved connection, names SHARD_REMOTE when it still beats the local daemon, and shard setup when nothing local is set up. (SHARD-657, SHARD-662)
func TestRemoveDeletesTheSavedConnection(t *testing.T) {
	for _, tc := range []struct {
		name      string
		vars      map[string]string
		installed bool
		want      []string
	}{
		{name: "local", installed: true, want: []string{"✓ Connection removed", "", "Shard commands now use the local daemon."}},
		{name: "nothing local", want: []string{"✓ Connection removed", "", "Shard commands now use this machine, which is not set up to run sandboxes.", "Run shard setup again to set it up."}},
		{name: "SHARD_REMOTE", vars: map[string]string{client.RemoteEnv: "https://other.example.com"}, want: []string{
			"✓ Connection removed", "", "SHARD_REMOTE is still set to https://other.example.com, and it overrides the local default.", "Unset it to use the local daemon.",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, path := testHost(t, tc.vars)
			saveConnection(t, path, client.Config{Remote: "https://shard.example.com", APIKey: testKey})
			if tc.installed {
				put(t, host, shardBinary, nil)
			}
			ui := &fakeUI{selects: map[Question]string{AskSaved: "remove"}}

			if err := (&Setup{Host: host, UI: ui}).remote(t.Context()); err != nil {
				t.Fatalf("remote: %v", err)
			}

			if got := savedConnection(t, path); got != (client.Config{}) {
				t.Errorf("the connection is still saved: %v", got)
			}
			if got := ui.printed[2:]; !slices.Equal(got, tc.want) {
				t.Errorf("printed %q, want %q", got, tc.want)
			}
		})
	}
}

// Exit leaves the saved connection as it is and checks nothing. (SHARD-657)
func TestExitLeavesTheSavedConnection(t *testing.T) {
	host, path := testHost(t, nil)
	saved := client.Config{Remote: "https://shard.example.com", APIKey: testKey}
	saveConnection(t, path, saved)
	ui := &fakeUI{selects: map[Question]string{AskSaved: "exit"}}

	if err := (&Setup{Host: host, UI: ui}).remote(t.Context()); err != nil {
		t.Fatalf("remote: %v", err)
	}

	if len(ui.lists) != 0 || savedConnection(t, path) != saved {
		t.Errorf("exit checked or changed the saved connection")
	}
}

// A switch to local asks now and removes the saved connection only when finish runs, after local setup succeeds. (SHARD-657)
func TestSwitchToLocalRemovesTheConnectionOnlyAtTheEnd(t *testing.T) {
	saved := client.Config{Remote: "https://shard.example.com", APIKey: testKey}

	for _, tc := range []struct {
		name   string
		remove bool
		want   client.Config
		says   string
	}{
		{name: "remove", remove: true, want: client.Config{}, says: "Shard commands now use the local daemon."},
		{name: "keep", want: saved, says: "The saved connection remains, so normal Shard commands still use the remote server."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, path := testHost(t, nil)
			saveConnection(t, path, saved)
			ui := &fakeUI{confirms: map[Question]bool{AskSwitch: tc.remove}}
			s := &Setup{Host: host, UI: ui}

			finish, err := s.switchToLocal(t.Context())
			if err != nil {
				t.Fatalf("switchToLocal: %v", err)
			}
			containsAll(t, ui.printed, "Normal Shard commands currently use the remote server https://shard.example.com, saved in "+path+".")
			if got := savedConnection(t, path); got != saved {
				t.Fatalf("the connection changed before local setup finished: %v", got)
			}

			put(t, host, shardBinary, nil)
			if err := finish(t.Context()); err != nil {
				t.Fatalf("finish: %v", err)
			}
			if got := savedConnection(t, path); got != tc.want {
				t.Errorf("after local setup the saved connection is %v, want %v", got, tc.want)
			}
			containsAll(t, ui.printed, tc.says)
			noLeak(t, ui, nil)
		})
	}
}

// With no saved connection a switch asks nothing, and says only that SHARD_REMOTE still beats the local daemon. (SHARD-657)
func TestSwitchToLocalWithNoSavedConnection(t *testing.T) {
	for _, tc := range []struct {
		name string
		vars map[string]string
		want []string
	}{
		{name: "quiet"},
		{name: "SHARD_REMOTE", vars: map[string]string{client.RemoteEnv: "https://shard.example.com"}, want: []string{
			"SHARD_REMOTE is still set to https://shard.example.com, and it overrides the local default.", "Unset it to use the local daemon.",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, _ := testHost(t, tc.vars)
			ui := &fakeUI{}
			finish, err := (&Setup{Host: host, UI: ui}).switchToLocal(t.Context())
			if err != nil {
				t.Fatalf("switchToLocal: %v", err)
			}
			if err := finish(t.Context()); err != nil {
				t.Fatalf("finish: %v", err)
			}

			if len(ui.asked) != 0 || !slices.Equal(ui.printed, tc.want) {
				t.Errorf("asked %v and printed %q, want nothing asked and %q", ui.asked, ui.printed, tc.want)
			}
		})
	}
}
