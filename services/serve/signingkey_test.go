package serve

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// helperRootEnv names the root a re-exec of this test binary creates the signing key under, for the cross-process race.
const helperRootEnv = "SHARD_TEST_SIGNING_KEY_ROOT"

func TestSigningKeyCreatesTheDefaultAt0700And0600(t *testing.T) {
	root := t.TempDir()

	key, path, err := SigningKey(root, "")
	if err != nil {
		t.Fatalf("SigningKey: %v", err)
	}
	if want := filepath.Join(root, "auth", "signing-key"); path != want {
		t.Errorf("the key is at %s, want %s", path, want)
	}
	if _, err := hex.DecodeString(string(key)); err != nil || len(key) != 64 {
		t.Errorf("the key is %d bytes and hex %v, want 64 hex characters", len(key), err == nil)
	}

	assertMode(t, filepath.Join(root, "auth"), 0o700)
	assertMode(t, path, 0o600)
	assertOnlyEntry(t, filepath.Join(root, "auth"), "signing-key")
}

// A root the daemon never made is created as the daemon creates it, so mint before the first daemon start works.
func TestSigningKeyCreatesAMissingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")

	if _, _, err := SigningKey(root, ""); err != nil {
		t.Fatalf("SigningKey: %v", err)
	}
	assertMode(t, filepath.Join(root, "auth"), 0o700)
}

func TestSigningKeyReusesTheKeyItCreated(t *testing.T) {
	root := t.TempDir()

	first, path, err := SigningKey(root, "")
	if err != nil {
		t.Fatalf("SigningKey: %v", err)
	}
	before := statOf(t, path)

	second, _, err := SigningKey(root, "")
	if err != nil {
		t.Fatalf("SigningKey again: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Error("the second call answered a different key")
	}
	if after := statOf(t, path); !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Errorf("the second call touched the key file: mtime %s then %s", before.ModTime(), after.ModTime())
	}
}

// An admin's key and the mode of an auth dir that exists stay as the admin left them.
func TestSigningKeyReusesAKeyAnAdminWrote(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "auth")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	path := filepath.Join(dir, "signing-key")
	if err := os.WriteFile(path, []byte(testSecret+"\n"), 0o640); err != nil {
		t.Fatalf("write the key: %v", err)
	}

	key, _, err := SigningKey(root, "")
	if err != nil {
		t.Fatalf("SigningKey: %v", err)
	}
	if string(key) != testSecret {
		t.Error("SigningKey answered a key other than the one on disk")
	}
	assertMode(t, dir, 0o750)
	assertMode(t, path, 0o640)
}

// invalidKey is a key file on disk and the words its refusal must carry.
type invalidKey struct {
	value string
	mode  fs.FileMode
	want  string
}

func TestSigningKeyRefusesAnInvalidKeyAndLeavesItAlone(t *testing.T) {
	cases := map[string]invalidKey{
		"short":          {value: strings.Repeat("k", 31), mode: 0o600, want: "31 bytes"},
		"empty":          {value: "  \n", mode: 0o600, want: "holds no key"},
		"world-readable": {value: testSecret, mode: 0o604, want: "everyone on the host can read"},
	}
	// Root reads a 0000 file, so only another user sees it as unreadable.
	if os.Geteuid() != 0 {
		cases["unreadable"] = invalidKey{value: testSecret, mode: 0o000, want: "permission denied"}
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := writeDefaultKey(t, root, c.value, c.mode)

			_, _, err := SigningKey(root, "")
			if err == nil {
				t.Fatal("SigningKey accepted the key")
			}
			assertRefusal(t, err, path, c.value, c.want)
			assertMode(t, path, c.mode)
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatalf("chmod: %v", err)
			}
			if held, err := os.ReadFile(path); err != nil || string(held) != c.value {
				t.Errorf("the refused key file now holds other bytes, or cannot be read: %v", err)
			}
		})
	}
}

func TestSigningKeyRefusesADirectoryAtTheKeyPath(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "auth", "signing-key")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	_, _, err := SigningKey(root, "")
	if err == nil {
		t.Fatal("SigningKey accepted a directory")
	}
	assertRefusal(t, err, path, "", "not a regular file")
}

func TestSigningKeyRefusesAShortExplicitKeyWithTheGenerationLine(t *testing.T) {
	path := secretFile(t, strings.Repeat("a", 31))

	_, _, err := SigningKey(t.TempDir(), path)
	if err == nil {
		t.Fatal("SigningKey accepted a 31-byte key")
	}
	assertRefusal(t, err, path, strings.Repeat("a", 31), "openssl rand -hex 32")

	for _, ok := range []string{strings.Repeat("a", 32), strings.Repeat("0123456789abcdef", 4)} {
		if _, _, err := SigningKey(t.TempDir(), secretFile(t, ok)); err != nil {
			t.Errorf("a %d-byte key was refused: %v", len(ok), err)
		}
	}
}

func TestSigningKeyRefusesAMissingExplicitFileAndCreatesNothing(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(t.TempDir(), "signing-key")

	_, _, err := SigningKey(root, missing)
	if err == nil {
		t.Fatal("SigningKey accepted a key file that does not exist")
	}
	assertRefusal(t, err, missing, "", "--signing-key-file")
	if _, err := os.Stat(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("SigningKey created the named file: %v", err)
	}
	assertNoAuthDir(t, root)
}

func TestSigningKeyPathIsTheDefaultOrTheNamedFile(t *testing.T) {
	root := t.TempDir()

	path, err := SigningKeyPath(root, "")
	if err != nil || path != filepath.Join(root, "auth", "signing-key") {
		t.Errorf("SigningKeyPath answered %q, %v, want the default under the root", path, err)
	}
	if TokensPath(path, "") != filepath.Join(root, "auth", "serve.tokens") {
		t.Errorf("the ledger beside the default key is %s", TokensPath(path, ""))
	}
	assertNoAuthDir(t, root)

	named := secretFile(t, testSecret)
	if path, err := SigningKeyPath(root, named); err != nil || path != named {
		t.Errorf("SigningKeyPath answered %q, %v, want the named file", path, err)
	}
}

func TestConcurrentFirstUseLandsOneKey(t *testing.T) {
	root := t.TempDir()

	const callers = 32
	keys := make([][]byte, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() { keys[i], _, errs[i] = SigningKey(root, "") })
	}
	wg.Wait()

	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if !bytes.Equal(keys[i], keys[0]) {
			t.Fatalf("caller %d answered a different key than caller 0", i)
		}
	}
	assertOnlyEntry(t, filepath.Join(root, "auth"), "signing-key")
}

// Under a umask that strips every bit, the creator's chmod is all that opens the auth directory, so a loser must wait for it.
func TestConcurrentFirstUseUnderATightUmaskLandsOneKey(t *testing.T) {
	const rounds, callers = 20, 32
	roots := make([]string, rounds)
	for i := range roots {
		roots[i] = t.TempDir()
	}

	old := syscall.Umask(0o777)
	t.Cleanup(func() { syscall.Umask(old) })

	for _, root := range roots {
		errs := make([]error, callers)
		var wg sync.WaitGroup
		for i := range callers {
			wg.Go(func() { _, _, errs[i] = SigningKey(root, "") })
		}
		wg.Wait()

		for i := range callers {
			if errs[i] != nil {
				t.Fatalf("caller %d: %v", i, errs[i])
			}
		}
		assertMode(t, filepath.Join(root, "auth"), 0o700)
	}
}

// The loser of a first-use race meets the winner's key at the link and reads it, and never replaces it.
func TestCreateSigningKeyKeepsAKeyThatLandedFirst(t *testing.T) {
	root := t.TempDir()
	path := writeDefaultKey(t, root, testSecret, 0o600)

	key, err := createSigningKey(path)
	if err != nil {
		t.Fatalf("createSigningKey: %v", err)
	}
	if string(key) != testSecret {
		t.Error("createSigningKey answered its own key, not the one that landed first")
	}
	assertOnlyEntry(t, filepath.Join(root, "auth"), "signing-key")
}

// Processes race the same way a mint and a serve started together do, through the file system alone.
func TestConcurrentFirstUseAcrossProcessesLandsOneKey(t *testing.T) {
	root := t.TempDir()

	const procs = 8
	outs := make([]bytes.Buffer, procs)
	cmds := make([]*exec.Cmd, procs)
	for i := range procs {
		cmds[i] = exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSigningKeyHelperProcess$")
		cmds[i].Env = append(os.Environ(), helperRootEnv+"="+root)
		cmds[i].Stdout = &outs[i]
	}
	for i := range procs {
		if err := cmds[i].Start(); err != nil {
			t.Fatalf("start process %d: %v", i, err)
		}
	}
	for i := range procs {
		if err := cmds[i].Wait(); err != nil {
			t.Fatalf("process %d: %v: %s", i, err, outs[i].String())
		}
	}

	key, _, err := SigningKey(root, "")
	if err != nil {
		t.Fatalf("SigningKey: %v", err)
	}
	for i := range procs {
		if !strings.Contains(outs[i].String(), "key="+fingerprint(key)+"\n") {
			t.Errorf("process %d did not answer the key on disk", i)
		}
	}
	assertOnlyEntry(t, filepath.Join(root, "auth"), "signing-key")
}

// TestSigningKeyHelperProcess is the body of one racing process; it does nothing in a plain run.
func TestSigningKeyHelperProcess(t *testing.T) {
	root := os.Getenv(helperRootEnv)
	if root == "" {
		return
	}

	key, _, err := SigningKey(root, "")
	if err != nil {
		t.Fatalf("SigningKey: %v", err)
	}
	fmt.Printf("key=%s\n", fingerprint(key))
}

func TestRevokeOnNoLedgerCreatesNothing(t *testing.T) {
	root := t.TempDir()
	path := TokensPath(filepath.Join(root, "auth", "signing-key"), "")

	found, err := RevokeToken(path, "no-such-id")
	if err != nil || found != 0 {
		t.Errorf("RevokeToken answered %d, %v, want 0 and no error", found, err)
	}
	if _, err := RevokeSubject(path, "ci"); err != nil {
		t.Errorf("RevokeSubject: %v", err)
	}
	assertNoAuthDir(t, root)
}

func TestTheFrontCreatesTheDefaultSigningKey(t *testing.T) {
	cert, key := keyPair(t)
	root := shortRoot(t)

	if _, err := New(Config{Listen: "127.0.0.1:0", CertFile: cert, KeyFile: key, Root: root}); err != nil {
		t.Fatalf("New: %v", err)
	}
	assertMode(t, filepath.Join(root, "auth", "signing-key"), 0o600)
}

func TestTheFrontRefusesAMissingSigningKeyFile(t *testing.T) {
	cert, key := keyPair(t)
	root := shortRoot(t)

	_, err := New(Config{Listen: "127.0.0.1:0", CertFile: cert, KeyFile: key, SigningKeyFile: filepath.Join(root, "missing"), Root: root})
	if err == nil {
		t.Fatal("the front started with a named key file that does not exist")
	}
	assertRefusal(t, err, filepath.Join(root, "missing"), "", "--signing-key-file")
	assertNoAuthDir(t, root)
}

// fingerprint stands in for a key in test output, so no key value is printed.
func fingerprint(key []byte) string {
	sum := sha256.Sum256(key)

	return hex.EncodeToString(sum[:8])
}

func writeDefaultKey(t *testing.T, root, value string, mode fs.FileMode) string {
	t.Helper()

	path := filepath.Join(root, "auth", "signing-key")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatalf("write the key: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	return path
}

func assertRefusal(t *testing.T, err error, path, value, want string) {
	t.Helper()

	msg := err.Error()
	if !strings.Contains(msg, path) || !strings.Contains(msg, want) {
		t.Errorf("the refusal is %q, want it to name %s and say %q", msg, path, want)
	}
	if strings.TrimSpace(value) != "" && strings.Contains(msg, strings.TrimSpace(value)) {
		t.Error("the refusal carries the key value")
	}
}

func assertMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()

	if got := statOf(t, path).Mode().Perm(); got != want {
		t.Errorf("%s is at mode %04o, want %04o", path, got, want)
	}
}

func assertOnlyEntry(t *testing.T, dir, name string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	if len(entries) != 1 || entries[0].Name() != name {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("%s holds %v, want only %s", dir, names, name)
	}
}

func assertNoAuthDir(t *testing.T, root string) {
	t.Helper()

	if _, err := os.Stat(filepath.Join(root, "auth")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s/auth exists, want nothing created: %v", root, err)
	}
}

func statOf(t *testing.T, path string) fs.FileInfo {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}

	return info
}
