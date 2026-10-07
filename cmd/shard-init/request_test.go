package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/supervisor"
)

func admitAll(net.Conn) error { return nil }

// ask sends one request over the process socket's protocol, as the daemon's exec of the helper does, and reads the one answer.
func ask(g *testGuest, admit func(net.Conn) error, m supervisor.Message) (reply supervisor.Message, err error) {
	caller, pid1 := net.Pipe()
	defer func() { err = errors.Join(err, caller.Close()) }()
	go g.serveRequest(pid1, admit)
	if err := caller.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return reply, fmt.Errorf("bound the request: %w", err)
	}
	if err := supervisor.WriteMessage(caller, m); err != nil {
		return reply, fmt.Errorf("send the request: %w", err)
	}
	if err := supervisor.ReadHeader(caller, &reply); err != nil {
		return reply, fmt.Errorf("read the answer: %w", err)
	}

	return reply, nil
}

func TestTheProcessSocketRunsAndStopsANamedProcess(t *testing.T) {
	g, report := startGuest(t)

	run := named("web", "term:0")
	reply, err := ask(g, admitAll, supervisor.Message{Kind: supervisor.KindRun, ID: 1, Run: &run})
	if err != nil || reply.Kind != supervisor.KindDone || reply.ID != 1 {
		t.Fatalf("the run answered %+v, %v; want done", reply, err)
	}
	report.await(t, "web", models.ProcessRunning)

	reply, err = ask(g, admitAll, supervisor.Message{Kind: supervisor.KindRun, ID: 2, Run: &run})
	if err != nil || reply.Kind != supervisor.KindFailure || !reply.Taken {
		t.Fatalf("a second run answered %+v, %v; want taken", reply, err)
	}

	reply, err = ask(g, admitAll, supervisor.Message{Kind: supervisor.KindStopProcess, ID: 3, Name: "web", Grace: 10 * time.Second})
	if err != nil || reply.Kind != supervisor.KindDone || reply.ID != 3 {
		t.Fatalf("the stop answered %+v, %v; want done", reply, err)
	}
	// The stop answers once the process is reaped, so the status is there already.
	if last := report.of("web"); last[len(last)-1].State != models.ProcessKilled {
		t.Fatalf("web reads %+v once the stop answered, want killed", last[len(last)-1])
	}
}

func TestTheProcessSocketRefusesWhatItDoesNotTake(t *testing.T) {
	g, _ := startGuest(t)

	cases := map[string]supervisor.Message{
		"another kind":     {Kind: supervisor.KindFreeze, ID: 4},
		"a run of nothing": {Kind: supervisor.KindRun, ID: 5},
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			reply, err := ask(g, admitAll, m)
			if err != nil || reply.Kind != supervisor.KindFailure || reply.ID != m.ID || reply.Error == "" {
				t.Fatalf("%s answered %+v, %v; want a failure", m.Kind, reply, err)
			}
		})
	}
}

// A caller admit refuses reads why, and nothing runs.
func TestTheProcessSocketAnswersACallerItRefuses(t *testing.T) {
	g, _ := startGuest(t)

	run := named("web", "say:hi")
	refuse := func(net.Conn) error { return errors.New("only root sends process requests") }
	reply, err := ask(g, refuse, supervisor.Message{Kind: supervisor.KindRun, ID: 1, Run: &run})
	if err != nil || reply.Kind != supervisor.KindFailure || reply.ID != 1 || reply.Error != "only root sends process requests" {
		t.Fatalf("a refused caller got %+v, %v; want the refusal", reply, err)
	}
	if names := g.procsNamed(); len(names) != 0 {
		t.Fatalf("a refused caller ran %q", names)
	}
}

// A stop waits out its grace on its own goroutine, so a run on another connection answers meanwhile.
func TestAStopWaitingOutItsGraceHoldsNoOtherRequest(t *testing.T) {
	g, report := startGuest(t)

	mustRun(t, g, named("stubborn", "ignoreterm"))
	waitFor(t, 5*time.Second, "stubborn to ignore TERM", func() bool { return report.log(t, "stubborn") == "ready\n" })
	stopped := make(chan supervisor.Message, 1)
	go func() {
		reply, err := ask(g, admitAll, supervisor.Message{Kind: supervisor.KindStopProcess, ID: 1, Name: "stubborn", Grace: 2 * time.Second})
		if err != nil {
			t.Errorf("the stop: %v", err)
		}
		stopped <- reply
	}()

	run := named("other", "say:hi")
	reply, err := ask(g, admitAll, supervisor.Message{Kind: supervisor.KindRun, ID: 2, Run: &run})
	if err != nil || reply.Kind != supervisor.KindDone {
		t.Fatalf("a run during the stop answered %+v, %v", reply, err)
	}
	select {
	case reply := <-stopped:
		t.Fatalf("the stop answered %+v before its grace ran out", reply)
	default:
	}
	select {
	case reply := <-stopped:
		if reply.Kind != supervisor.KindDone {
			t.Fatalf("the stop answered %+v", reply)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stop never answered")
	}
}

// A PID 1 that never bound the socket predates named processes, which the helper tells apart from any other failure.
func TestForwardToAPID1WithNoSocketIsOutdated(t *testing.T) {
	missing := filepath.Join(shortDir(t), "missing.sock")
	stale := filepath.Join(shortDir(t), "stale.sock")
	l, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	unixListener, ok := l.(*net.UnixListener)
	if !ok {
		t.Fatalf("a unix listen gave a %T", l)
	}
	// A socket file nobody listens on is what a dead listener leaves, and its dial is ECONNREFUSED.
	unixListener.SetUnlinkOnClose(false)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	for name, addr := range map[string]string{"no socket": missing, "nobody listening": stale} {
		t.Run(name, func(t *testing.T) {
			setRequestAddr(t, addr)
			reply, err := forward(supervisor.Message{Kind: supervisor.KindRun, ID: 6})
			if err != nil {
				t.Fatalf("forward: %v", err)
			}
			if reply.Kind != supervisor.KindFailure || !reply.Outdated || reply.ID != 6 || reply.Error != errOutdated.Error() {
				t.Errorf("the answer is %+v, want an outdated failure", reply)
			}
		})
	}
}

func setRequestAddr(t *testing.T, addr string) {
	t.Helper()

	was := requestAddr
	requestAddr = addr
	t.Cleanup(func() { requestAddr = was })
}

// The helper prints whatever PID 1 answered and exits 0, so the daemon reads the answer and never a guessed exit code.
func TestRunRequestPrintsTheAnswer(t *testing.T) {
	setRequestAddr(t, filepath.Join(shortDir(t), "missing.sock"))

	var request, stdout bytes.Buffer
	if err := supervisor.WriteMessage(&request, supervisor.Message{Kind: supervisor.KindStopProcess, ID: 8, Name: "web"}); err != nil {
		t.Fatal(err)
	}
	if code := runRequest(&request, &stdout); code != 0 {
		t.Fatalf("runRequest = %d, want 0", code)
	}
	var reply supervisor.Message
	if err := supervisor.ReadHeader(&stdout, &reply); err != nil {
		t.Fatalf("read the printed answer: %v", err)
	}
	if !reply.Outdated || reply.ID != 8 {
		t.Errorf("the printed answer is %+v, want outdated for request 8", reply)
	}

	if code := runRequest(strings.NewReader("not a request"), &stdout); code != 1 {
		t.Errorf("runRequest of a bad request = %d, want 1", code)
	}
}

// Over a live socket the helper hands the request to PID 1 and prints its answer.
func TestRunRequestReachesAListeningPID1(t *testing.T) {
	g, report := startGuest(t)
	addr := filepath.Join(shortDir(t), "process.sock")
	setRequestAddr(t, addr)
	l, err := net.Listen("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := l.Close(); err != nil {
			t.Error(err)
		}
	})
	go g.acceptRequests(l, admitAll)

	run := named("web", "say:hi")
	var request, stdout bytes.Buffer
	if err := supervisor.WriteMessage(&request, supervisor.Message{Kind: supervisor.KindRun, ID: 1, Run: &run}); err != nil {
		t.Fatal(err)
	}
	if code := runRequest(&request, &stdout); code != 0 {
		t.Fatalf("runRequest = %d, want 0", code)
	}
	var reply supervisor.Message
	if err := supervisor.ReadHeader(&stdout, &reply); err != nil || reply.Kind != supervisor.KindDone {
		t.Fatalf("the printed answer is %+v, %v; want done", reply, err)
	}
	report.await(t, "web", models.ProcessExited)
}

// Only root the runtime entered from outside the sandbox has no parent in it; a status read after the reap names no one.
func TestAdmitTakesOnlyRootTheHostEntered(t *testing.T) {
	cases := []struct {
		name   string
		uid    uint32
		status string
		want   string
	}{
		{"the host's exec", 0, "Name:\tinit\nPid:\t7\nPPid:\t0\n", ""},
		{"a sandbox user", 1000, "Pid:\t7\nPPid:\t0\n", "only root"},
		{"guest root born in the sandbox", 0, "Pid:\t7\nPPid:\t1\n", "caller 7 has parent 1"},
		{"a caller reaped mid-read", 0, "Pid:\t0\nPPid:\t0\n", "ended before its parent was read"},
		{"a status with no parent", 0, "Pid:\t7\n", "has no PPid"},
		{"a parent that is not a number", 0, "Pid:\t7\nPPid:\tx\n", "read PPid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := admit(c.uid, 7, []byte(c.status))
			if c.want == "" && err != nil {
				t.Fatalf("admit refused: %v", err)
			}
			if c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Fatalf("admit gave %v, want %q", err, c.want)
			}
		})
	}
}
