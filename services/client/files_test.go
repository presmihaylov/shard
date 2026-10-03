package client_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/sandbox"
)

const statJSON = `{"type":"file","size":5,"mode":420,"uid":1000,"gid":1000,"mtime":"2026-10-02T09:00:00Z"}`

func TestPutFileSendsTheLengthAndTheQuery(t *testing.T) {
	var asked, body string
	var length int64
	c := serve(t, shortRoot(t), func(w http.ResponseWriter, r *http.Request) {
		asked = r.Method + " " + r.URL.RequestURI()
		length = r.ContentLength
		read, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read the put: %v", err)
		}
		body = string(read)
		w.WriteHeader(http.StatusNoContent)
	})

	// An *os.File gives net/http no length of its own, which is the case the client must cover.
	src := filepath.Join(t.TempDir(), "app.conf")
	if err := os.WriteFile(src, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write the source: %v", err)
	}
	f, err := os.Open(src)
	if err != nil {
		t.Fatalf("open the source: %v", err)
	}
	t.Cleanup(func() { f.Close() })

	req := sandbox.FileWrite{Path: "/srv/app.conf", Mode: 0o600, User: "app", Parents: true, Size: 5}
	if err := c.PutFile(t.Context(), "sb1", req, f); err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if want := "PUT /v0/sandboxes/sb1/files?mode=600&parents=true&path=%2Fsrv%2Fapp.conf&user=app"; asked != want {
		t.Errorf("the client asked %s, want %s", asked, want)
	}
	if length != 5 || body != "hello" {
		t.Errorf("the daemon got %q with a length of %d, want hello of 5", body, length)
	}
}

func TestPutFileOfNothingStillSendsALength(t *testing.T) {
	var length int64
	c := serve(t, shortRoot(t), func(w http.ResponseWriter, r *http.Request) {
		length = r.ContentLength
		w.WriteHeader(http.StatusNoContent)
	})

	if err := c.PutFile(t.Context(), "sb1", sandbox.FileWrite{Path: "/srv/empty", Mode: 0o644}, strings.NewReader("")); err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if length != 0 {
		t.Errorf("the daemon got a length of %d, want 0", length)
	}
}

func TestPutFileKeepsTheDaemonsRefusal(t *testing.T) {
	c := serve(t, shortRoot(t), answer(http.StatusNotFound, `{"error":{"code":"not_found","message":"put /srv/x/y: no such file or directory"}}`))

	err := c.PutFile(t.Context(), "sb1", sandbox.FileWrite{Path: "/srv/x/y", Mode: 0o644, Size: 1}, strings.NewReader("x"))
	var refused *client.APIError
	if !errors.As(err, &refused) || refused.Code != models.CodeNotFound || !strings.Contains(err.Error(), "/srv/x/y") {
		t.Fatalf("PutFile gave %v, want the daemon's not_found naming the path", err)
	}
	var missing *client.NotFoundError
	if errors.As(err, &missing) {
		t.Fatalf("a missing file read as a missing sandbox: %v", err)
	}
}

func TestGetFileAnswersTheStatAndTheBytes(t *testing.T) {
	c := serve(t, shortRoot(t), func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Query().Get("path") != "/srv/app.conf" {
			t.Errorf("the client asked %s %s", r.Method, r.URL.RequestURI())
		}
		w.Header().Set(api.StatHeader, statJSON)
		w.Header().Set("Content-Length", "5")
		_, _ = w.Write([]byte("hello"))
	})

	stat, body, err := c.GetFile(t.Context(), "sb1", "/srv/app.conf")
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	t.Cleanup(func() { body.Close() })
	want := models.FileStat{Type: models.FileRegular, Size: 5, Mode: 0o644, UID: 1000, GID: 1000, MTime: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)}
	if stat != want {
		t.Errorf("the stat is %+v, want %+v", stat, want)
	}
	got, err := io.ReadAll(body)
	if err != nil || string(got) != "hello" {
		t.Errorf("the body is %q, %v, want hello", got, err)
	}
}

func TestGetFileKeepsTheDaemonsRefusal(t *testing.T) {
	c := serve(t, shortRoot(t), answer(http.StatusBadRequest, `{"error":{"code":"invalid_request","message":"get /srv: is a directory"}}`))

	_, _, err := c.GetFile(t.Context(), "sb1", "/srv")
	var refused *client.APIError
	if !errors.As(err, &refused) || refused.Code != models.CodeInvalidRequest || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("GetFile gave %v, want the daemon's invalid_request", err)
	}
}

func TestStatFileReadsTheHeader(t *testing.T) {
	c := serve(t, shortRoot(t), func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("the client asked %s, want HEAD", r.Method)
		}
		w.Header().Set(api.StatHeader, statJSON)
	})

	stat, err := c.StatFile(t.Context(), "sb1", "/srv/app.conf")
	if err != nil || stat.Type != models.FileRegular || stat.Size != 5 {
		t.Fatalf("StatFile = %+v, %v; want a file of 5 bytes", stat, err)
	}
}

func TestStatFileRefusalCarriesTheStatus(t *testing.T) {
	c := serve(t, shortRoot(t), answer(http.StatusNotFound, ""))

	_, err := c.StatFile(t.Context(), "sb1", "/srv/missing")
	var refused *client.APIError
	if !errors.As(err, &refused) || refused.Status != http.StatusNotFound {
		t.Fatalf("StatFile gave %v, want an API error with status 404", err)
	}
}

func TestListDirAnswersTheEntries(t *testing.T) {
	var asked string
	c := serve(t, shortRoot(t), func(w http.ResponseWriter, r *http.Request) {
		asked = r.Method + " " + r.URL.RequestURI()
		_, _ = w.Write([]byte(`{"entries":[{"name":"app","type":"file","size":5,"mode":420,"uid":0,"gid":0,"mtime":"2026-10-02T09:00:00Z"}]}`))
	})

	entries, err := c.ListDir(t.Context(), "sb1", "/srv/my app")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if want := "GET /v0/sandboxes/sb1/ls?path=%2Fsrv%2Fmy+app"; asked != want {
		t.Errorf("the client asked %s, want %s", asked, want)
	}
	want := models.FileEntry{Name: "app", FileStat: models.FileStat{Type: models.FileRegular, Size: 5, Mode: 0o644, MTime: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)}}
	if len(entries) != 1 || entries[0] != want {
		t.Fatalf("the entries are %+v, want %+v", entries, want)
	}
}

// The daemon leaves off the closing bracket of a listing the guest cut, so the client fails rather than answer part of it.
func TestListDirOfACutListingFails(t *testing.T) {
	c := serve(t, shortRoot(t), answer(http.StatusOK, `{"entries":[{"name":"app","type":"file"}`))

	if entries, err := c.ListDir(t.Context(), "sb1", "/srv"); err == nil {
		t.Fatalf("a cut listing answered %+v", entries)
	}
}

func TestMakeDirSendsTheBody(t *testing.T) {
	var asked string
	var got sandbox.MkdirRequest
	c := serve(t, shortRoot(t), func(w http.ResponseWriter, r *http.Request) {
		asked = r.Method + " " + r.URL.RequestURI()
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode the mkdir: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	req := sandbox.MkdirRequest{Path: "/srv/a/b", Mode: "700", Parents: true, User: "app"}
	if err := c.MakeDir(t.Context(), "sb1", req); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	if asked != "POST /v0/sandboxes/sb1/mkdir" || got != req {
		t.Fatalf("the client asked %s with %+v, want POST /v0/sandboxes/sb1/mkdir with %+v", asked, got, req)
	}
}

func TestDeleteFileSendsRecursiveOnlyWhenAsked(t *testing.T) {
	cases := []struct {
		recursive bool
		want      string
	}{
		{recursive: false, want: "DELETE /v0/sandboxes/sb1/files?path=%2Fsrv%2Fcache"},
		{recursive: true, want: "DELETE /v0/sandboxes/sb1/files?path=%2Fsrv%2Fcache&recursive=true"},
	}
	for _, c := range cases {
		var asked string
		cl := serve(t, shortRoot(t), func(w http.ResponseWriter, r *http.Request) {
			asked = r.Method + " " + r.URL.RequestURI()
			w.WriteHeader(http.StatusNoContent)
		})

		if err := cl.DeleteFile(t.Context(), "sb1", "/srv/cache", c.recursive); err != nil {
			t.Fatalf("DeleteFile: %v", err)
		}
		if asked != c.want {
			t.Errorf("the client asked %s, want %s", asked, c.want)
		}
	}
}

// not_found on a directory verb is the guest path, never the sandbox, so the client keeps the daemon's words.
func TestDeleteFileKeepsTheDaemonsRefusal(t *testing.T) {
	c := serve(t, shortRoot(t), answer(http.StatusNotFound, `{"error":{"code":"not_found","message":"delete /srv/cache: no such file or directory"}}`))

	err := c.DeleteFile(t.Context(), "sb1", "/srv/cache", false)
	var refused *client.APIError
	if !errors.As(err, &refused) || refused.Code != models.CodeNotFound || !strings.Contains(err.Error(), "/srv/cache") {
		t.Fatalf("DeleteFile gave %v, want the daemon's not_found naming the path", err)
	}
	var missing *client.NotFoundError
	if errors.As(err, &missing) {
		t.Fatalf("a missing guest path read as a missing sandbox: %v", err)
	}
}
