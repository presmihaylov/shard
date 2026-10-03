package api_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

func archiveRequest(t *testing.T, s seeded, method, query string, body io.Reader) *http.Response {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, s.server.URL+"/v0/sandboxes/"+s.running.ID+"/archive?"+query, body)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	resp, err := s.server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s /archive: %v", method, err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	return resp
}

// A put of an archive streams, so a body with no Content-Length is the usual case and not a refusal.
func TestPutArchiveStreamsTheBodyAsTheQuerySays(t *testing.T) {
	s := seed(t)

	resp := archiveRequest(t, s, http.MethodPut, "path=/srv&user=app", io.NopCloser(strings.NewReader("a tar"))) //nolint:bodyclose // archiveRequest closes the body in a cleanup
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("the put answered %d, want 204", resp.StatusCode)
	}
	want := sandbox.ArchiveWrite{Path: "/srv", User: "app"}
	if s.verbs.fileOp != "unpack" || s.verbs.archive != want || s.verbs.landed != "a tar" || s.verbs.ref != s.running.ID {
		t.Fatalf("the put reached the orchestrator as a %s of %+v with %q on %s, want an unpack of %+v with the body on %s", s.verbs.fileOp, s.verbs.archive, s.verbs.landed, s.verbs.ref, want, s.running.ID)
	}
}

func TestAPutArchiveOutlivesTheReadTimeoutWhileItsBodyMoves(t *testing.T) {
	s := slow(t, seed(t), 200*time.Millisecond)

	status := trickle(t, s, "archive?path=/srv", slices.Repeat([]time.Duration{20 * time.Millisecond}, 20))
	if status != http.StatusNoContent || s.verbs.landed != strings.Repeat("x", 20) {
		t.Fatalf("a body that moved for 400 ms answered %d and landed %q, want 204 and all 20 bytes", status, s.verbs.landed)
	}
}

// The SHARD-338 bound still holds on an archive: a client that stops sending cannot hold the unpack open.
func TestAPutArchiveWhoseBodyStallsIsCut(t *testing.T) {
	s := slow(t, seed(t), 200*time.Millisecond)

	status := trickle(t, s, "archive?path=/srv", append(slices.Repeat([]time.Duration{20 * time.Millisecond}, 15), time.Second))
	if status == http.StatusNoContent || s.verbs.landed != strings.Repeat("x", 15) {
		t.Fatalf("a body that stalled for 1 s answered %d and landed %q, want a failure after the 15 bytes before the stall", status, s.verbs.landed)
	}
}

func TestGetArchiveStreamsTheTarWithItsStat(t *testing.T) {
	s := seed(t)
	s.verbs.stat = models.FileStat{Type: models.FileDir, Mode: 0o755, MTime: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)}
	s.verbs.content = "a tar"

	resp := archiveRequest(t, s, http.MethodGet, "path=/srv/app", nil) //nolint:bodyclose // archiveRequest closes the body in a cleanup
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/x-tar" {
		t.Fatalf("the get answered %d %s, want 200 application/x-tar", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if got := statOf(t, resp); got != s.verbs.stat {
		t.Fatalf("the stat header is %+v, want %+v", got, s.verbs.stat)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "a tar" {
		t.Fatalf("the body is %q, %v, want the tar", body, err)
	}
	if s.verbs.fileOp != "pack" || s.verbs.filePath != "/srv/app" {
		t.Fatalf("the orchestrator got a %s of %s, want a pack of /srv/app", s.verbs.fileOp, s.verbs.filePath)
	}
}

// A tar can end cleanly at any entry, so a guest that dies midway must reach the client as a cut stream, never a clean end.
func TestGetArchiveThatDiesMidwayAbortsTheResponse(t *testing.T) {
	s := seed(t)
	s.verbs.stat = models.FileStat{Type: models.FileDir}
	// Past the response buffer, so the 200 is out before the cut.
	s.verbs.content = strings.Repeat("t", 64<<10)
	s.verbs.bodyErr = errors.New("the guest went away")
	s.verbs.closedErr = errors.New("/.shard/init files exited 1")

	resp := archiveRequest(t, s, http.MethodGet, "path=/srv", nil) //nolint:bodyclose // archiveRequest closes the body in a cleanup
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the get answered %d, want 200", resp.StatusCode)
	}
	if _, err := io.ReadAll(resp.Body); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("reading the cut tar gave %v, want io.ErrUnexpectedEOF", err)
	}
}

// A client that read the trailer closes the body, and the hang-up that shuts the exec is the end of a whole copy, not a failure (SHARD-410).
func TestAClientThatHangsUpAfterTheTrailerLogsNoFailure(t *testing.T) {
	s := seed(t)
	s.verbs.stat = models.FileStat{Type: models.FileDir}
	s.verbs.content = wholeTar(t)
	s.verbs.hangUpErr = &fs.PathError{Op: "read", Path: "|0", Err: os.ErrClosed}

	body := archiveStream(t, s)
	tr := tar.NewReader(body)
	for {
		_, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("the trailer never reached the client: %v", err)
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			t.Fatalf("read an entry: %v", err)
		}
	}
	if err := body.Close(); err != nil {
		t.Fatalf("close the body: %v", err)
	}

	// The close waits for the handler, so the log is final.
	s.server.Close()
	if logged := s.log.String(); logged != "" {
		t.Fatalf("the daemon logged %q after a whole copy, want nothing", logged)
	}
}

// A tar can end at any entry, so a client that hangs up before the trailer was cut, even at an entry boundary, and the daemon says so.
func TestAClientThatHangsUpBeforeTheTrailerStillLogs(t *testing.T) {
	s := seed(t)
	s.verbs.stat = models.FileStat{Type: models.FileDir}
	whole := wholeTar(t)
	s.verbs.content = whole[:len(whole)-1024]
	s.verbs.hangUpErr = &fs.PathError{Op: "read", Path: "|0", Err: os.ErrClosed}

	body := archiveStream(t, s)
	if _, err := tar.NewReader(body).Next(); err != nil {
		t.Fatalf("read the first header: %v", err)
	}
	if err := body.Close(); err != nil {
		t.Fatalf("close the body: %v", err)
	}

	// The response write can see the hang-up before the exec does, so either side's error is the cut.
	s.server.Close()
	if logged := s.log.String(); !strings.Contains(logged, "api: archive /work from sandbox "+s.running.ID+": ") {
		t.Fatalf("the daemon logged %q after a cut copy, want the failure", logged)
	}
}

// Only the client's own hang-up ends a whole tar quietly: an exec that fails after the trailer still logs and cuts the response.
func TestAFailureAfterTheTrailerStillLogs(t *testing.T) {
	s := seed(t)
	s.verbs.stat = models.FileStat{Type: models.FileDir}
	s.verbs.content = wholeTar(t)
	s.verbs.bodyErr = errors.New("the guest went away")

	if _, err := io.ReadAll(archiveStream(t, s)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("reading the tar gave %v, want io.ErrUnexpectedEOF", err)
	}

	s.server.Close()
	if logged := s.log.String(); !strings.Contains(logged, "the guest went away") {
		t.Fatalf("the daemon logged %q, want the guest's failure", logged)
	}
}

// wholeTar is one file and the trailer, 64 KiB in all, so the last of the server's 32 KiB reads is big enough to reach the client unbuffered.
func wholeTar(t *testing.T) string {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	data := bytes.Repeat([]byte("w"), 64<<10-3*512)
	if err := tw.WriteHeader(&tar.Header{Name: "work/blob", Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("write the header: %v", err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatalf("write the data: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("write the trailer: %v", err)
	}
	if buf.Len() != 64<<10 {
		t.Fatalf("the tar is %d bytes, want 64 KiB", buf.Len())
	}

	return buf.String()
}

// archiveStream opens a get of /work and answers its body, bounded so a trailer the server holds back fails the test rather than hanging it.
func archiveStream(t *testing.T, s seeded) io.ReadCloser {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.server.URL+"/v0/sandboxes/"+s.running.ID+"/archive?path=/work", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	resp, err := s.server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /archive: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the get answered %d, want 200", resp.StatusCode)
	}

	return resp.Body
}

func TestArchiveRefusalsAnswerTheirCodes(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   models.Code
	}{
		{err: &sandbox.FileNotFoundError{Err: errors.New("pack /srv/missing: no such file or directory")}, status: http.StatusNotFound, code: models.CodeNotFound},
		{err: &sandbox.RequestError{Err: errors.New(`unpack /srv: refuse the entry "../x": a .. component`)}, status: http.StatusBadRequest, code: models.CodeInvalidRequest},
		{err: errors.New("/.shard/init files exited 1"), status: http.StatusInternalServerError, code: models.CodeInternal},
	}
	for _, c := range cases {
		t.Run(string(c.code), func(t *testing.T) {
			s := seed(t)
			s.verbs.err = c.err

			resp := archiveRequest(t, s, http.MethodGet, "path=/srv/x", nil) //nolint:bodyclose // archiveRequest closes the body in a cleanup
			if resp.StatusCode != c.status || refusalOf(t, resp).code != string(c.code) {
				t.Fatalf("the get answered %d, want %d %s", resp.StatusCode, c.status, c.code)
			}
			if resp := archiveRequest(t, s, http.MethodPut, "path=/srv", strings.NewReader("")); resp.StatusCode != c.status { //nolint:bodyclose // archiveRequest closes the body in a cleanup
				t.Fatalf("the put answered %d, want %d", resp.StatusCode, c.status)
			}
		})
	}
}
