package vzvm

import (
	"errors"
	"io/fs"
	"slices"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
)

const (
	seedPasswd = "root:x:0:0:root:/root:/bin/sh\nreviewer:x:1001:1001::/home/reviewer:/bin/bash\n"
	seedRoots  = "-----BEGIN CERTIFICATE-----\nseed\n-----END CERTIFICATE-----\n"
	proxyCA    = "-----BEGIN CERTIFICATE-----\nproxy\n-----END CERTIFICATE-----\n"
)

// seedFiles stands in for the booted guest: it reads the seed's files by path, and nothing else is there.
func seedFiles(files map[string]string) bundle.ReadGuest {
	return func(rel string) ([]byte, fs.FileMode, error) {
		content, found := files[rel]
		if !found {
			return nil, 0, &fs.PathError{Op: "get", Path: "/" + rel, Err: fs.ErrNotExist}
		}

		return []byte(content), 0, nil
	}
}

// seededSpec is a create from a snapshot over an image that holds neither the user nor any roots.
func seededSpec(t *testing.T) models.SandboxSpec {
	return models.SandboxSpec{
		ID: "sb-1", RootFS: t.TempDir(), Seed: t.TempDir(), Entrypoint: []string{"/bin/true"},
		User: "reviewer", ProxyCA: []byte(proxyCA),
	}
}

// The image cannot answer for a seed, and the guest refuses a bare name, so a create cut before launch never runs as root.
func TestASeededRecordLeavesTheUserForTheGuestsFiles(t *testing.T) {
	r, err := recordOf(seededSpec(t))
	if err != nil {
		t.Fatalf("recordOf read the image for a seeded sandbox: %v", err)
	}
	if r.Run.User != "reviewer" || r.Run.Trust != nil {
		t.Errorf("recorded user %q and trust %+v, want the bare name and no trust yet", r.Run.User, r.Run.Trust)
	}
}

// A user and roots added before the snapshot are on the seed's disk alone (SHARD-784).
func TestASeededRecordResolvesAgainstTheSeedsFiles(t *testing.T) {
	spec := seededSpec(t)
	r, err := recordOf(spec)
	if err != nil {
		t.Fatal(err)
	}

	r, err = seeded(r, spec, seedFiles(map[string]string{"etc/passwd": seedPasswd, "etc/ssl/certs/ca-certificates.crt": seedRoots}))
	if err != nil {
		t.Fatalf("seeded: %v", err)
	}
	if r.Run.User != "1001:1001" {
		t.Errorf("recorded user %q, want 1001:1001", r.Run.User)
	}
	if r.Run.Trust == nil || r.Run.Trust.Path != "/etc/ssl/certs/ca-certificates.crt" || string(r.Run.Trust.Roots) != seedRoots+proxyCA {
		t.Errorf("recorded trust %+v, want the seed's roots and the proxy CA", r.Run.Trust)
	}
	if !slices.Contains(r.Run.Env, "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt") {
		t.Errorf("recorded env %v, want SSL_CERT_FILE on the seed's bundle", r.Run.Env)
	}

	// A later policy attach reads the roots the create kept, as the image holds none.
	other := "-----BEGIN CERTIFICATE-----\nrotated\n-----END CERTIFICATE-----\n"
	if err := r.trust([]byte(other)); err != nil {
		t.Fatalf("trust after the create: %v", err)
	}
	if string(r.Run.Trust.Roots) != seedRoots+other {
		t.Errorf("trust after the create holds %q, want the seed's roots and the new CA alone", r.Run.Trust.Roots)
	}
}

func TestASeedWithoutRootsRefusesAProxyCA(t *testing.T) {
	spec := seededSpec(t)
	r, err := recordOf(spec)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := seeded(r, spec, seedFiles(map[string]string{"etc/passwd": seedPasswd})); !errors.Is(err, bundle.ErrNoCABundle) {
		t.Errorf("seeded = %v, want %v", err, bundle.ErrNoCABundle)
	}
}
