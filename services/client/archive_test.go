package client_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/tarball"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/client"
)

func TestPutArchiveStreamsTheTarChunked(t *testing.T) {
	var asked, body string
	var length int64
	var encoding []string
	c := serve(t, shortRoot(t), func(w http.ResponseWriter, r *http.Request) {
		asked = r.Method + " " + r.URL.RequestURI()
		length, encoding = r.ContentLength, r.TransferEncoding
		read, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read the put: %v", err)
		}
		body = string(read)
		w.WriteHeader(http.StatusNoContent)
	})

	if err := c.PutArchive(t.Context(), "sb1", "/srv", "app", strings.NewReader("a tar")); err != nil {
		t.Fatalf("PutArchive: %v", err)
	}
	if want := "PUT /v0/sandboxes/sb1/archive?path=%2Fsrv&user=app"; asked != want {
		t.Errorf("the client asked %s, want %s", asked, want)
	}
	if length != -1 || !slices.Equal(encoding, []string{"chunked"}) || body != "a tar" {
		t.Errorf("the daemon got %q with a length of %d and %v, want a tar chunked", body, length, encoding)
	}
}

func TestPutArchiveKeepsTheDaemonsRefusal(t *testing.T) {
	c := serve(t, shortRoot(t), answer(http.StatusBadRequest, `{"error":{"code":"invalid_request","message":"unpack into /srv: refuse the entry \"../x\": a name that leaves the destination"}}`))

	err := c.PutArchive(t.Context(), "sb1", "/srv", "", strings.NewReader("a tar"))
	var refused *client.APIError
	if !errors.As(err, &refused) || refused.Code != models.CodeInvalidRequest || !strings.Contains(err.Error(), "../x") {
		t.Fatalf("PutArchive gave %v, want the daemon's invalid_request naming the entry", err)
	}
}

func TestGetArchiveAnswersTheStatAndTheTar(t *testing.T) {
	c := serve(t, shortRoot(t), func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v0/sandboxes/sb1/archive" || r.URL.Query().Get("path") != "/srv" {
			t.Errorf("the client asked %s %s", r.Method, r.URL.RequestURI())
		}
		w.Header().Set(api.StatHeader, `{"type":"dir","mode":493}`)
		_, _ = w.Write([]byte("a tar"))
	})

	stat, body, err := c.GetArchive(t.Context(), "sb1", "/srv")
	if err != nil {
		t.Fatalf("GetArchive: %v", err)
	}
	t.Cleanup(func() { body.Close() })
	if stat.Type != models.FileDir || stat.Mode != 0o755 {
		t.Errorf("the stat is %+v, want a dir of 0755", stat)
	}
	got, err := io.ReadAll(body)
	if err != nil || string(got) != "a tar" {
		t.Errorf("the body is %q, %v, want a tar", got, err)
	}
}

func TestGetArchiveKeepsTheDaemonsRefusal(t *testing.T) {
	c := serve(t, shortRoot(t), answer(http.StatusNotFound, `{"error":{"code":"not_found","message":"pack /srv/missing: no such file or directory"}}`))

	_, _, err := c.GetArchive(t.Context(), "sb1", "/srv/missing")
	var refused *client.APIError
	if !errors.As(err, &refused) || refused.Code != models.CodeNotFound {
		t.Fatalf("GetArchive gave %v, want the daemon's not_found", err)
	}
}

// hostile is a tar as a sandbox could send it: the writer takes any header, whatever the host would make of it.
func hostile(t *testing.T, headers ...*tar.Header) io.Reader {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, hdr := range headers {
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write %s: %v", hdr.Name, err)
		}
		if hdr.Typeflag == tar.TypeReg && hdr.Size <= 1<<10 {
			if _, err := tw.Write(bytes.Repeat([]byte("x"), int(hdr.Size))); err != nil {
				t.Fatalf("write %s: %v", hdr.Name, err)
			}
		}
	}

	return &buf
}

// The sandbox sends the tar a copy out unpacks, so nothing it names may land outside dst or past the caps.
func TestUnpackArchiveRefusesAHostileGuest(t *testing.T) {
	dir := &tar.Header{Name: "srv/", Typeflag: tar.TypeDir, Mode: 0o755}
	under := &tar.Header{Name: "srv/link/planted", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}
	cases := []struct {
		name string
		hdrs []*tar.Header
	}{
		{name: "an entry that climbs out", hdrs: []*tar.Header{{Name: "srv/../outside/x", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}}},
		{name: "an entry outside the source", hdrs: []*tar.Header{{Name: "etc/passwd", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}}},
		{name: "a symlink out", hdrs: []*tar.Header{{Name: "srv/link", Typeflag: tar.TypeSymlink, Linkname: "../../outside"}}},
		{name: "an absolute symlink", hdrs: []*tar.Header{{Name: "srv/link", Typeflag: tar.TypeSymlink, Linkname: "/etc"}}},
		{name: "a symlink out and an entry under it", hdrs: []*tar.Header{{Name: "srv/link", Typeflag: tar.TypeSymlink, Linkname: "../../outside"}, under}},
		// On a case-insensitive host, srv/link/planted resolves through srv/LINK (CVE-2021-21300).
		{name: "two names that differ only in case", hdrs: []*tar.Header{{Name: "srv/LINK", Typeflag: tar.TypeSymlink, Linkname: "../../outside"}, under}},
		{name: "a hard link out", hdrs: []*tar.Header{{Name: "srv/hard", Typeflag: tar.TypeLink, Linkname: "../outside/secret"}}},
		{name: "a device node", hdrs: []*tar.Header{{Name: "srv/disk", Typeflag: tar.TypeBlock, Mode: 0o600}}},
		// The header alone claims the size, so the cap holds before a byte of the body is read.
		{name: "past the byte cap", hdrs: []*tar.Header{{Name: "srv/big", Typeflag: tar.TypeReg, Mode: 0o644, Size: client.MaxArchiveBytes + 1}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parent := t.TempDir()
			dst, outside := filepath.Join(parent, "dst"), filepath.Join(parent, "outside")
			for _, d := range []string{dst, outside} {
				if err := os.Mkdir(d, 0o755); err != nil {
					t.Fatalf("make %s: %v", d, err)
				}
			}
			if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("host"), 0o600); err != nil {
				t.Fatalf("write the secret: %v", err)
			}

			err := client.UnpackArchive(hostile(t, append([]*tar.Header{dir}, c.hdrs...)...), dst, "srv")
			var refused *tarball.RefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("the unpack gave %v, want a refusal", err)
			}
			left, err := os.ReadDir(outside)
			if err != nil || len(left) != 1 {
				t.Fatalf("outside dst holds %v, %v, want the secret alone", left, err)
			}
			links, err := filepath.Glob(filepath.Join(dst, "[lL][iI][nN][kK]"))
			if err != nil || len(links) != 0 {
				t.Fatalf("the refused link stayed at %v, %v", links, err)
			}
		})
	}
}

// A guest that sends a header past the entry cap is refused there, so a tar of empty entries cannot run the host on forever.
func TestUnpackArchiveRefusesPastTheEntryCap(t *testing.T) {
	t.Parallel()

	// A global header is one block the unpack skips with no syscall, so the stream reaches the cap fast.
	var one bytes.Buffer
	if err := tar.NewWriter(&one).WriteHeader(&tar.Header{Name: "g", Typeflag: tar.TypeXGlobalHeader}); err != nil {
		t.Fatalf("write the header: %v", err)
	}
	const batch = 1 << 10
	chunk := bytes.Repeat(one.Bytes(), batch)
	pr, pw := io.Pipe()
	go func() {
		for range client.MaxArchiveEntries/batch + 1 {
			if _, err := pw.Write(chunk); err != nil {
				return
			}
		}
		pw.CloseWithError(errors.New("the guest ran out of entries before the cap"))
	}()
	t.Cleanup(func() { pr.Close() })

	err := client.UnpackArchive(pr, t.TempDir(), "")
	var refused *tarball.RefusedError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "entries") {
		t.Fatalf("the unpack gave %v, want the entry cap's refusal", err)
	}
}

func TestUnpackArchiveDropsSetid(t *testing.T) {
	dst := t.TempDir()

	err := client.UnpackArchive(hostile(t, &tar.Header{Name: "srv/", Typeflag: tar.TypeDir, Mode: 0o755}, &tar.Header{Name: "srv/run", Typeflag: tar.TypeReg, Mode: 0o6755, Size: 1}), dst, "srv")
	if err != nil {
		t.Fatalf("UnpackArchive: %v", err)
	}
	info, err := os.Stat(filepath.Join(dst, "run"))
	if err != nil || info.Mode() != 0o755 {
		t.Fatalf("the file landed as %v, %v, want 0755 with no setid", info, err)
	}
}
