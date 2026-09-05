package api_test

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/sandbox"
)

func TestFramesRoundTripBothWays(t *testing.T) {
	sent := []struct {
		stream  byte
		payload string
	}{
		{api.StreamStdin, "typed\n"},
		{api.StreamStdout, "hello\n"},
		{api.StreamStderr, "careful\n"},
		{api.StreamStdinClose, ""},
		{api.StreamExit, "7"},
	}

	var wire bytes.Buffer
	for _, frame := range sent {
		if err := api.WriteFrame(&wire, frame.stream, []byte(frame.payload)); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
	}

	for _, want := range sent {
		stream, payload, err := api.ReadFrame(&wire)
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		if stream != want.stream || string(payload) != want.payload {
			t.Errorf("read stream %d %q, want stream %d %q", stream, payload, want.stream, want.payload)
		}
	}

	if _, _, err := api.ReadFrame(&wire); !errors.Is(err, io.EOF) {
		t.Errorf("ReadFrame at the end returned %v, want io.EOF", err)
	}
}

// A write longer than one frame goes as several, and the reader hands each of them back whole.
func TestWriteFrameSplitsAPayloadOverTheLimit(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), api.MaxFrameSize+16)

	var wire bytes.Buffer
	if err := api.WriteFrame(&wire, api.StreamStdout, payload); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}

	var read []byte
	for range 2 {
		_, piece, err := api.ReadFrame(&wire)
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		read = append(read, piece...)
	}

	if !bytes.Equal(read, payload) {
		t.Errorf("the frames carried %d bytes, want %d", len(read), len(payload))
	}
}

// A length nobody meant must not make the reader allocate for it.
func TestReadFrameRefusesALengthNoFrameHas(t *testing.T) {
	header := []byte{api.StreamStdout, 0, 0, 0, 0xff, 0xff, 0xff, 0xff}

	if _, _, err := api.ReadFrame(bytes.NewReader(header)); err == nil {
		t.Fatal("ReadFrame accepted a frame of four gigabytes")
	}
}

// exec dials the server, asks for the upgrade itself, and hands back the connection and the exec id.
func exec(t *testing.T, s seeded, ref, body string) (net.Conn, *bufio.Reader, string) {
	t.Helper()

	conn, err := net.Dial("tcp", strings.TrimPrefix(s.server.URL, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.server.URL+"/v0/sandboxes/"+ref+"/exec", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")

	if err := req.Write(conn); err != nil {
		t.Fatalf("write the request: %v", err)
	}

	reader := bufio.NewReader(conn)

	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("the daemon answered %d, want 101", resp.StatusCode)
	}

	return conn, reader, resp.Header.Get(api.ExecIDHeader)
}

// read collects the frames until the exit frame, which is what ends every exec.
func read(t *testing.T, r io.Reader) (out, errOut, failure string, code string) {
	t.Helper()

	for {
		stream, payload, err := api.ReadFrame(r)
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}

		switch stream {
		case api.StreamStdout:
			out += string(payload)
		case api.StreamStderr:
			errOut += string(payload)
		case api.StreamError:
			failure = string(payload)
		case api.StreamExit:
			return out, errOut, failure, string(payload)
		default:
			t.Fatalf("the daemon sent a frame of stream %d", stream)
		}
	}
}

func TestExecUpgradesAndCarriesTheCommandBothWays(t *testing.T) {
	s := seed(t)
	s.verbs.execID = "1a2b3c4d5e6f7a8b"
	s.verbs.out = "hello\n"
	s.verbs.errOut = "careful\n"
	s.verbs.exit = models.ExitStatus{Code: 7}

	conn, reader, execID := exec(t, s, s.running.ID, `{"command":["sh","-c","exit 7"],"env":["A=1"],"stdin":true}`)

	if execID != "1a2b3c4d5e6f7a8b" {
		t.Errorf("the 101 named exec %q", execID)
	}

	if err := api.WriteFrame(conn, api.StreamStdin, []byte("typed\n")); err != nil {
		t.Fatalf("write the stdin frame: %v", err)
	}
	if err := api.WriteFrame(conn, api.StreamStdinClose, nil); err != nil {
		t.Fatalf("write the stdin-close frame: %v", err)
	}

	out, errOut, failure, code := read(t, reader)
	if out != "hello\n" || errOut != "careful\n" {
		t.Errorf("stdout = %q, stderr = %q", out, errOut)
	}
	if failure != "" {
		t.Errorf("the daemon reported %q, and the command ran", failure)
	}
	if code != "7" {
		t.Errorf("the exit frame carried %q, want 7", code)
	}
	if s.verbs.input != "typed\n" {
		t.Errorf("the command read %q, want typed", s.verbs.input)
	}
	if want := []string{"sh", "-c", "exit 7"}; strings.Join(s.verbs.exec.Command, " ") != strings.Join(want, " ") {
		t.Errorf("command = %v, want %v", s.verbs.exec.Command, want)
	}
}

// Nothing is on the wire before the command runs, so a refusal is a status and a JSON body like any other.
func TestExecRefusesBeforeTheUpgrade(t *testing.T) {
	s := seed(t)
	s.verbs.err = &sandbox.StateError{ID: s.stopped.ID, State: models.StateStopped, Fix: "start it again with shard start " + s.stopped.ID}

	code, body := send(t, s.server, http.MethodPost, "/v0/sandboxes/"+s.stopped.ID+"/exec", `{"command":["true"]}`)
	if code != http.StatusConflict {
		t.Fatalf("the daemon answered %d, want 409", code)
	}
	if answer, _ := body["error"].(string); !strings.Contains(answer, "shard start") {
		t.Errorf("the refusal is %q, and it must say what to do", answer)
	}
}

// A command that never ran carries both frames, so the client answers with the code a shell answers.
func TestExecReportsACommandThatNeverRan(t *testing.T) {
	s := seed(t)
	s.verbs.execErr = &models.CommandNotStartedError{Sandbox: s.running.ID, Reason: "failed to load /bin/nope: no such file or directory", Code: 127}

	_, reader, _ := exec(t, s, s.running.ID, `{"command":["/bin/nope"]}`)

	_, _, failure, code := read(t, reader)
	if failure != "failed to load /bin/nope: no such file or directory" {
		t.Errorf("the error frame carried %q, want the reason alone", failure)
	}
	if code != "127" {
		t.Errorf("the exit frame carried %q, want 127", code)
	}
}

func TestResizeNamesTheExecAndAnswers204(t *testing.T) {
	s := seed(t)

	code, _ := send(t, s.server, http.MethodPost, "/v0/sandboxes/"+s.running.ID+"/exec/1a2b3c4d5e6f7a8b/resize", `{"rows":24,"cols":80}`)
	if code != http.StatusNoContent {
		t.Fatalf("the daemon answered %d, want 204", code)
	}
	if s.verbs.resizedExec != "1a2b3c4d5e6f7a8b" {
		t.Errorf("the daemon resized exec %q", s.verbs.resizedExec)
	}
	if s.verbs.size != (sandbox.TerminalSize{Rows: 24, Cols: 80}) {
		t.Errorf("size = %+v, want 24 by 80", s.verbs.size)
	}
}

func TestLogsAnswerAsPlainText(t *testing.T) {
	s := seed(t)
	s.verbs.lines = []string{"hello\n", "world\n"}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.server.URL+"/v0/sandboxes/"+s.running.ID+"/logs", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}

	resp, err := s.server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET the output: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the output: %v", err)
	}
	if string(body) != "hello\nworld\n" {
		t.Errorf("the output is %q", body)
	}
}

// A follow arrives as it happens and ends when the sandbox stops, with nothing left for the client to do.
func TestLogsFollowEndsWhenTheSandboxStops(t *testing.T) {
	s := seed(t)
	s.verbs.lines = []string{"first\n"}
	s.verbs.stops = make(chan struct{})

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.server.URL+"/v0/sandboxes/"+s.running.ID+"/logs?follow=true", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}

	resp, err := s.server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET the output: %v", err)
	}
	defer resp.Body.Close()

	// The first line is on the wire before the sandbox stops, which is what a flush on every write buys.
	first := make([]byte, len("first\n"))
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatalf("read the first line: %v", err)
	}
	if string(first) != "first\n" {
		t.Errorf("the follow began with %q", first)
	}

	close(s.verbs.stops)

	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the rest: %v", err)
	}
	if len(rest) != 0 {
		t.Errorf("the follow went on with %q", rest)
	}
	if !s.verbs.followed {
		t.Error("the daemon was never told to follow")
	}
}
