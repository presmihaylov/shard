package api_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/sandbox"
)

func fileRequest(t *testing.T, s seeded, method, query string, body io.Reader) *http.Response {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, s.server.URL+"/v0/sandboxes/"+s.running.ID+"/files?"+query, body)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	resp, err := s.server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s /files: %v", method, err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	return resp
}

func refusalOf(t *testing.T, resp *http.Response) refused {
	t.Helper()

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode the refusal: %v", err)
	}

	return errorOf(t, body)
}

func statOf(t *testing.T, resp *http.Response) models.FileStat {
	t.Helper()

	var stat models.FileStat
	if err := json.Unmarshal([]byte(resp.Header.Get(api.StatHeader)), &stat); err != nil {
		t.Fatalf("decode %s %q: %v", api.StatHeader, resp.Header.Get(api.StatHeader), err)
	}

	return stat
}

func TestPutFileLandsTheBodyAsTheQuerySays(t *testing.T) {
	s := seed(t)

	resp := fileRequest(t, s, http.MethodPut, "path=/srv/app.conf&mode=0600&user=app&parents=true", strings.NewReader("hello")) //nolint:bodyclose // fileRequest closes the body in a cleanup
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("the put answered %d, want 204", resp.StatusCode)
	}

	want := sandbox.FileWrite{Path: "/srv/app.conf", Mode: 0o600, User: "app", Parents: true, Size: 5}
	if s.verbs.file != want || s.verbs.landed != "hello" || s.verbs.ref != s.running.ID {
		t.Fatalf("the put reached the orchestrator as %+v with %q on %s, want %+v with hello on %s", s.verbs.file, s.verbs.landed, s.verbs.ref, want, s.running.ID)
	}
}

func TestPutFileDefaultsTheModeTo0644(t *testing.T) {
	s := seed(t)

	if resp := fileRequest(t, s, http.MethodPut, "path=/srv/app.conf", strings.NewReader("")); resp.StatusCode != http.StatusNoContent { //nolint:bodyclose // fileRequest closes the body in a cleanup
		t.Fatalf("the put answered %d, want 204", resp.StatusCode)
	}
	if s.verbs.file.Mode != 0o644 || s.verbs.file.User != "" || s.verbs.file.Parents {
		t.Fatalf("the put named %+v, want mode 0644 as the entrypoint user with no parents", s.verbs.file)
	}
}

func TestPutFileRefusesWhatItCannotRead(t *testing.T) {
	cases := []struct {
		name  string
		query string
		body  io.Reader
	}{
		// A reader the client cannot measure goes out chunked, with no Content-Length.
		{name: "no length", query: "path=/srv/app.conf", body: io.NopCloser(strings.NewReader("hello"))},
		{name: "a mode that is not octal", query: "path=/srv/app.conf&mode=0x1ff", body: strings.NewReader("")},
		{name: "parents that is not a bool", query: "path=/srv/app.conf&parents=maybe", body: strings.NewReader("")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := seed(t)

			resp := fileRequest(t, s, http.MethodPut, c.query, c.body) //nolint:bodyclose // fileRequest closes the body in a cleanup
			if resp.StatusCode != http.StatusBadRequest || refusalOf(t, resp).code != string(models.CodeInvalidRequest) {
				t.Fatalf("the put answered %d, want 400 invalid_request", resp.StatusCode)
			}
			if s.verbs.fileOp != "" {
				t.Fatalf("the refusal still reached the orchestrator: %s", s.verbs.fileOp)
			}
		})
	}
}

// trickle sends a put by hand, one byte after each pause, since the client transport holds a sized body back in its buffer.
func trickle(t *testing.T, s seeded, pauses []time.Duration) int {
	t.Helper()

	conn, err := net.Dial("tcp", s.server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial the server: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	if _, err := fmt.Fprintf(conn, "PUT /v0/sandboxes/%s/files?path=/srv/big HTTP/1.1\r\nHost: shard\r\nContent-Length: %d\r\n\r\n", s.running.ID, len(pauses)); err != nil {
		t.Fatalf("send the head: %v", err)
	}
	for _, pause := range pauses {
		time.Sleep(pause)
		// A server that cut the body may already have closed the connection; its answer says why.
		if _, err := conn.Write([]byte("x")); err != nil {
			break
		}
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	return resp.StatusCode
}

// A put's body streams past the ReadTimeout while it keeps moving, so a large file is never cut at the bound.
func TestAPutOutlivesTheReadTimeoutWhileItsBodyMoves(t *testing.T) {
	s := slow(t, seed(t), 200*time.Millisecond)

	status := trickle(t, s, slices.Repeat([]time.Duration{20 * time.Millisecond}, 20))
	if status != http.StatusNoContent || s.verbs.landed != strings.Repeat("x", 20) {
		t.Fatalf("a body that moved for 400 ms answered %d and landed %q, want 204 and all 20 bytes", status, s.verbs.landed)
	}
}

// The ReadTimeout still bounds a body that stalls, so a client that stops sending cannot hold the put open.
func TestAPutWhoseBodyStallsIsCut(t *testing.T) {
	s := slow(t, seed(t), 200*time.Millisecond)

	status := trickle(t, s, append(slices.Repeat([]time.Duration{20 * time.Millisecond}, 15), time.Second))
	if status == http.StatusNoContent || s.verbs.landed != strings.Repeat("x", 15) {
		t.Fatalf("a body that stalled for 1 s answered %d and landed %q, want a failure after the 15 bytes before the stall", status, s.verbs.landed)
	}
}

func TestGetFileStreamsTheFileWithItsStat(t *testing.T) {
	s := seed(t)
	s.verbs.stat = models.FileStat{Type: models.FileRegular, Size: 5, Mode: 0o755, UID: 1000, GID: 1000, MTime: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)}
	s.verbs.content = "hello"

	resp := fileRequest(t, s, http.MethodGet, "path=/srv/run.sh", nil) //nolint:bodyclose // fileRequest closes the body in a cleanup
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/octet-stream" || !slices.Equal(resp.TransferEncoding, []string{"chunked"}) {
		t.Fatalf("the get answered %d %s with encoding %v, want 200 application/octet-stream, chunked", resp.StatusCode, resp.Header.Get("Content-Type"), resp.TransferEncoding)
	}
	if got := statOf(t, resp); got != s.verbs.stat {
		t.Fatalf("the stat header is %+v, want %+v", got, s.verbs.stat)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "hello" {
		t.Fatalf("the body is %q, %v, want hello", body, err)
	}
	if s.verbs.fileOp != "read" || s.verbs.filePath != "/srv/run.sh" {
		t.Fatalf("the orchestrator got a %s of %s, want a read of /srv/run.sh", s.verbs.fileOp, s.verbs.filePath)
	}
}

// A /proc file states a size of 0, so the body runs to the end of the stream and never stops at the stat.
func TestGetFileStreamsPastTheSizeItsStatStates(t *testing.T) {
	s := seed(t)
	s.verbs.stat = models.FileStat{Type: models.FileRegular, Size: 0}
	s.verbs.content = "Name:\tsh\nState:\tS (sleeping)\n"

	resp := fileRequest(t, s, http.MethodGet, "path=/proc/self/status", nil) //nolint:bodyclose // fileRequest closes the body in a cleanup
	body, err := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || err != nil || string(body) != s.verbs.content {
		t.Fatalf("the get answered %d with %q, %v, want 200 with the whole stream", resp.StatusCode, body, err)
	}
}

// A guest that dies midway cuts the chunked body before its last chunk, which the client reads as an error.
func TestGetFileThatDiesMidwayCutsTheBody(t *testing.T) {
	s := seed(t)
	s.verbs.stat = models.FileStat{Type: models.FileRegular, Size: 10}
	s.verbs.content = "hello"
	s.verbs.bodyErr = errors.New("the guest went away")
	s.verbs.closedErr = errors.New("/.shard/init files exited 1")

	resp := fileRequest(t, s, http.MethodGet, "path=/srv/blob", nil) //nolint:bodyclose // fileRequest closes the body in a cleanup
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the get answered %d, want 200", resp.StatusCode)
	}
	if _, err := io.ReadAll(resp.Body); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("reading the cut body gave %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestHeadFileAnswersTheStatAlone(t *testing.T) {
	s := seed(t)
	s.verbs.stat = models.FileStat{Type: models.FileDir, Mode: 0o1777, MTime: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)}

	resp := fileRequest(t, s, http.MethodHead, "path=/tmp", nil) //nolint:bodyclose // fileRequest closes the body in a cleanup
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the head answered %d, want 200", resp.StatusCode)
	}
	if got := statOf(t, resp); got != s.verbs.stat {
		t.Fatalf("the stat header is %+v, want %+v", got, s.verbs.stat)
	}
	if s.verbs.fileOp != "stat" || s.verbs.filePath != "/tmp" {
		t.Fatalf("the orchestrator got a %s of %s, want a stat of /tmp", s.verbs.fileOp, s.verbs.filePath)
	}
}

func TestFileRefusalsAnswerTheirCodes(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   models.Code
	}{
		{err: &sandbox.FileNotFoundError{Err: errors.New("get /srv/missing: no such file or directory")}, status: http.StatusNotFound, code: models.CodeNotFound},
		{err: &sandbox.RequestError{Err: errors.New("get /srv: is a directory")}, status: http.StatusBadRequest, code: models.CodeInvalidRequest},
		{err: &sandbox.StateError{ID: "sandbox1", State: models.StateStopped, Fix: "start it", Code: models.CodeSandboxNotRunning}, status: http.StatusConflict, code: models.CodeSandboxNotRunning},
		{err: errors.New("/.shard/init files exited 1"), status: http.StatusInternalServerError, code: models.CodeInternal},
	}
	for _, c := range cases {
		t.Run(string(c.code), func(t *testing.T) {
			s := seed(t)
			s.verbs.err = c.err

			resp := fileRequest(t, s, http.MethodGet, "path=/srv/x", nil) //nolint:bodyclose // fileRequest closes the body in a cleanup
			if resp.StatusCode != c.status || refusalOf(t, resp).code != string(c.code) {
				t.Fatalf("the get answered %d, want %d %s", resp.StatusCode, c.status, c.code)
			}
			if resp := fileRequest(t, s, http.MethodHead, "path=/srv/x", nil); resp.StatusCode != c.status { //nolint:bodyclose // fileRequest closes the body in a cleanup
				t.Fatalf("the head answered %d, want %d", resp.StatusCode, c.status)
			}
			if resp := fileRequest(t, s, http.MethodPut, "path=/srv/x", strings.NewReader("")); resp.StatusCode != c.status { //nolint:bodyclose // fileRequest closes the body in a cleanup
				t.Fatalf("the put answered %d, want %d", resp.StatusCode, c.status)
			}
		})
	}
}
