package bundle_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/runspec"
)

// reviewerPasswd is what `useradd reviewer` leaves in a sandbox a snapshot then keeps, which the image never holds.
const (
	reviewerPasswd = "root:x:0:0:root:/root:/bin/sh\nreviewer:x:1001:1001::/home/reviewer:/bin/bash\n"
	reviewerGroup  = "root:x:0:\nreviewer:x:1001:\n"
)

// seedWith lays out a snapshot's files: each path under its upper layer holds its content, and /tmp is empty.
func seedWith(t *testing.T, files map[string]string) string {
	t.Helper()

	seed := t.TempDir()
	if err := os.MkdirAll(filepath.Join(seed, "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		full := filepath.Join(seed, "upper", rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, full, content)
	}

	return seed
}

// A user added before the snapshot is in the seed's passwd, not the image's (SHARD-784).
func TestBuildResolvesAUserOnlyTheSeedHolds(t *testing.T) {
	spec := newSpec(t)
	spec.Seed = seedWith(t, map[string]string{"etc/passwd": reviewerPasswd, "etc/group": reviewerGroup})
	spec.User = "reviewer"

	_, got := build(t, spec, models.ImageConfig{})

	if want := "1001:1001"; userArg(t, got) != want {
		t.Errorf("got the user %q, want %q", userArg(t, got), want)
	}
}

// A CA bundle installed before the snapshot is in the seed, and the proxy CA joins it there (SHARD-784).
func TestBuildTrustsTheRootsOnlyTheSeedHolds(t *testing.T) {
	spec := newSpec(t)
	spec.Seed = seedWith(t, map[string]string{"etc/ssl/certs/ca-certificates.crt": imageRoots})
	spec.ProxyCA = []byte(proxyCA)

	b, got := build(t, spec, models.ImageConfig{})

	if envOf(t, got.Process.Env, "SSL_CERT_FILE") != "/etc/ssl/certs/ca-certificates.crt" {
		t.Errorf("SSL_CERT_FILE = %v", got.Process.Env)
	}
	if got := readFile(t, filepath.Join(b.Upper, "etc/ssl/certs/ca-certificates.crt")); got != imageRoots+proxyCA {
		t.Errorf("the merged bundle is:\n%s", got)
	}
}

// A seed that never touched the user databases or the roots shows the image's through its upper layer.
func TestBuildReadsTheImageUnderASeedThatLeftItAlone(t *testing.T) {
	spec := newSpec(t)
	if err := os.MkdirAll(filepath.Join(spec.RootFS, "etc/ssl/certs"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(spec.RootFS, "etc/ssl/certs/ca-certificates.crt"), imageRoots)
	spec.Seed = seedWith(t, map[string]string{"marker": "seeded\n"})
	spec.User = "app"
	spec.ProxyCA = []byte(proxyCA)

	b, got := build(t, spec, models.ImageConfig{})

	if want := "1000:2000"; userArg(t, got) != want {
		t.Errorf("got the user %q, want %q", userArg(t, got), want)
	}
	if got := readFile(t, filepath.Join(b.Upper, "etc/ssl/certs/ca-certificates.crt")); got != imageRoots+proxyCA {
		t.Errorf("the merged bundle is:\n%s", got)
	}
}

// A snapshot of a fronted sandbox already holds the proxy CA, so a create from it adds no second copy.
func TestBuildAddsTheProxyCAOnceToASeedThatHoldsIt(t *testing.T) {
	spec := newSpec(t)
	spec.Seed = seedWith(t, map[string]string{"etc/ssl/certs/ca-certificates.crt": imageRoots + proxyCA})
	spec.ProxyCA = []byte(proxyCA)

	b, _ := build(t, spec, models.ImageConfig{})

	if got := readFile(t, filepath.Join(b.Upper, "etc/ssl/certs/ca-certificates.crt")); got != imageRoots+proxyCA {
		t.Errorf("the merged bundle is:\n%s", got)
	}
}

func TestTrustProxyTwiceAddsTheProxyCAOnce(t *testing.T) {
	b := built(t)

	for range 2 {
		if err := b.TrustProxy([]byte(proxyCA)); err != nil {
			t.Fatalf("TrustProxy: %v", err)
		}
	}

	if got := readFile(t, filepath.Join(b.Upper, "etc/ssl/certs/ca-certificates.crt")); got != imageRoots+proxyCA {
		t.Errorf("the merged bundle is:\n%s", got)
	}
}

// A guest link in the seed resolves as the guest sees it, through the image below, whichever way it climbs.
func TestBuildFollowsASeedLinkIntoTheImage(t *testing.T) {
	for name, target := range map[string]string{
		"absolute": "/usr/lib/passwd",
		"relative": "../usr/lib/passwd",
		"climbing": "../../../../usr/./lib/../lib/passwd",
	} {
		t.Run(name, func(t *testing.T) {
			spec := newSpec(t)
			if err := os.MkdirAll(filepath.Join(spec.RootFS, "usr/lib"), 0o755); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(spec.RootFS, "usr/lib/passwd"), reviewerPasswd)
			spec.Seed = seedWith(t, map[string]string{"marker": "seeded\n"})
			if err := os.MkdirAll(filepath.Join(spec.Seed, "upper/etc"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(spec.Seed, "upper/etc/passwd")); err != nil {
				t.Fatal(err)
			}
			spec.User = "reviewer"

			_, got := build(t, spec, models.ImageConfig{})

			if want := "1001:1001"; userArg(t, got) != want {
				t.Errorf("got the user %q, want %q", userArg(t, got), want)
			}
		})
	}
}

// A seed link that names a host path finds the guest's path of that name, never the host file.
func TestBuildNeverFollowsASeedLinkOntoTheHost(t *testing.T) {
	host := filepath.Join(t.TempDir(), "passwd")
	write(t, host, reviewerPasswd)
	spec := newSpec(t)
	spec.Seed = seedWith(t, map[string]string{"marker": "seeded\n"})
	if err := os.MkdirAll(filepath.Join(spec.Seed, "upper/etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(host, filepath.Join(spec.Seed, "upper/etc/passwd")); err != nil {
		t.Fatal(err)
	}
	spec.User = "reviewer"

	if _, err := newService(t).Build(runspec.Resolve(spec, models.ImageConfig{})); err == nil || !strings.Contains(err.Error(), "no such entry") {
		t.Errorf("Build = %v, want the user not found, never read from a host file a seed link names", err)
	}
}

func TestGuestPathsReadTheUserDatabasesOnlyForAUser(t *testing.T) {
	roots := bundle.GuestPaths(nil, "")
	if slices.Contains(roots, "etc/passwd") || !slices.Contains(roots, "etc/ssl/certs/ca-certificates.crt") {
		t.Errorf("GuestPaths with no user = %v, want the CA candidates alone", roots)
	}
	named := bundle.GuestPaths([]string{"SSL_CERT_FILE=/opt/roots.pem"}, "reviewer")
	for _, want := range []string{"etc/passwd", "etc/group", "opt/roots.pem"} {
		if !slices.Contains(named, want) {
			t.Errorf("GuestPaths for a user = %v, want %s in it", named, want)
		}
	}
}

// guestFiles answers a read as a guest would: a mode alone for what is no file, fs.ErrNotExist for what is not there.
func guestFiles(files map[string]string, modes map[string]fs.FileMode) bundle.ReadGuest {
	return func(rel string) ([]byte, fs.FileMode, error) {
		if mode, found := modes[rel]; found {
			return nil, mode, nil
		}
		content, found := files[rel]
		if !found {
			return nil, 0, &fs.PathError{Op: "get", Path: "/" + rel, Err: fs.ErrNotExist}
		}

		return []byte(content), 0, nil
	}
}

func TestGuestTreeCopiesWhatTheGuestHolds(t *testing.T) {
	read := guestFiles(map[string]string{"etc/passwd": reviewerPasswd, "etc/ssl/certs/ca-certificates.crt": imageRoots}, nil)
	tree, err := bundle.GuestTree(bundle.GuestPaths(nil, "reviewer"), read)
	if err != nil {
		t.Fatalf("GuestTree: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(tree) })

	if got := readFile(t, filepath.Join(tree, "etc/passwd")); got != reviewerPasswd {
		t.Errorf("the tree's passwd is %q", got)
	}
	if _, err := os.Stat(filepath.Join(tree, "etc/group")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the tree holds a group the guest does not: %v", err)
	}
	roots, err := bundle.ReadRoots(tree, nil)
	if err != nil || roots.Path != "/etc/ssl/certs/ca-certificates.crt" || string(roots.PEM) != imageRoots {
		t.Errorf("ReadRoots = %+v, %v, want the guest's bundle", roots, err)
	}
}

// runc refuses a user database that is no file (SHARD-653), and a CA bundle that is no file is no bundle.
func TestGuestTreeRefusesOnlyAUserDatabaseThatIsNoFile(t *testing.T) {
	modes := map[string]fs.FileMode{"etc/passwd": fs.ModeIrregular, "etc/ssl/certs/ca-certificates.crt": fs.ModeIrregular}

	if _, err := bundle.GuestTree(bundle.GuestPaths(nil, "reviewer"), guestFiles(nil, modes)); err == nil || !strings.Contains(err.Error(), "/etc/passwd") {
		t.Errorf("GuestTree for a user over a passwd that is no file = %v, want a refusal naming it", err)
	}

	tree, err := bundle.GuestTree(bundle.GuestPaths(nil, ""), guestFiles(nil, modes))
	if err != nil {
		t.Fatalf("GuestTree with no user: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(tree) })
	roots, err := bundle.ReadRoots(tree, nil)
	if err != nil || len(roots.PEM) != 0 {
		t.Errorf("ReadRoots = %+v, %v, want no roots", roots, err)
	}
	if _, err := roots.Trust([]byte(proxyCA)); !errors.Is(err, bundle.ErrNoCABundle) {
		t.Errorf("Trust with no roots = %v, want %v", err, bundle.ErrNoCABundle)
	}
}

func TestRootsTrustEndsTheBundleOnALineOfItsOwn(t *testing.T) {
	roots := bundle.Roots{Path: "/etc/ssl/certs/ca-certificates.crt", PEM: []byte(strings.TrimSuffix(imageRoots, "\n"))}

	store, err := roots.Trust([]byte(proxyCA))
	if err != nil {
		t.Fatalf("Trust: %v", err)
	}
	if string(store.Roots) != imageRoots+proxyCA {
		t.Errorf("the merged bundle is:\n%s", store.Roots)
	}
	if !slices.Contains(store.Env, "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt") {
		t.Errorf("the store's env is %v", store.Env)
	}
}
