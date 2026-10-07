package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/presmihaylov/shard/services/supervisor"
)

// requestAddr is the abstract socket a container PID 1 takes process requests on; the sandbox's own network namespace scopes the name.
var requestAddr = "@shard-init/process"

// requestGrace bounds the wait for a caller's request line, so one that never sends holds no goroutine of PID 1.
const requestGrace = 10 * time.Second

// errOutdated is a PID 1 that takes no process requests, which only a stop and start replaces.
var errOutdated = errors.New("the sandbox's supervisor predates named processes: stop and start the sandbox, then run again")

// request does one process request: a run answers once the process forked, a stop-process once it is reaped.
func (g *guest) request(m supervisor.Message) supervisor.Message {
	switch m.Kind {
	case supervisor.KindRun:
		if m.Run == nil {
			return answerOf(m.ID, errors.New("a run message names no process"))
		}

		return answerOf(m.ID, g.runSpec(*m.Run))
	case supervisor.KindStopProcess:
		var reaped <-chan struct{}
		g.run(func() { reaped = g.stopProcess(m.Name, m.Grace) })
		<-reaped

		return answerOf(m.ID, nil)
	default:
		return answerOf(m.ID, fmt.Errorf("the host sent a %q request, which the process socket does not take", m.Kind))
	}
}

// acceptRequests serves each caller admit lets in on its own goroutine, so a stop that waits out its grace holds no other request.
func (g *guest) acceptRequests(l net.Listener, admit func(net.Conn) error) {
	for {
		conn, err := l.Accept()
		if err != nil {
			fmt.Fprintln(os.Stderr, "shard-init: accept a process request:", err)

			return
		}
		go g.serveRequest(conn, admit)
	}
}

// serveRequest takes one request line and answers it with one; PID 1 must survive a bad caller, so its failures are reported and never fatal (AGENTS.md).
func (g *guest) serveRequest(conn net.Conn, admit func(net.Conn) error) {
	defer conn.Close()

	// Judged at accept, before the caller sends, so a caller that hands its socket on cannot outwait the check.
	refused := admit(conn)
	if refused != nil {
		fmt.Fprintln(os.Stderr, "shard-init: refuse a process request:", refused)
	}
	if err := conn.SetReadDeadline(time.Now().Add(requestGrace)); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init: bound a process request:", err)

		return
	}
	var m supervisor.Message
	if err := supervisor.ReadHeader(conn, &m); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init: read a process request:", err)

		return
	}
	// A refusal is answered, so a host the check wrongly refuses reads why and not an outdated supervisor.
	reply := answerOf(m.ID, refused)
	if refused == nil {
		reply = g.request(m)
	}
	if err := supervisor.WriteMessage(conn, reply); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init: answer a process request:", err)
	}
}

// admit takes root with no parent in the sandbox's PID namespace: a fork in it always has one, and only the runtime's exec enters from outside.
func admit(uid uint32, pid int32, status []byte) error {
	if uid != 0 {
		return errors.New("only root sends process requests")
	}
	seen, err := statusField(status, "Pid")
	if err != nil {
		return err
	}
	// A caller reaped while its status was read prints pid 0, and that record says nothing of its parent.
	if seen != int(pid) {
		return fmt.Errorf("caller %d ended before its parent was read", pid)
	}
	parent, err := statusField(status, "PPid")
	if err != nil {
		return err
	}
	if parent != 0 {
		return fmt.Errorf("only the host's exec sends process requests, and caller %d has parent %d in the sandbox", pid, parent)
	}

	return nil
}

// statusField reads one number of a /proc/<pid>/status record.
func statusField(status []byte, key string) (int, error) {
	for line := range strings.Lines(string(status)) {
		name, value, ok := strings.Cut(line, ":")
		if !ok || name != key {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return 0, fmt.Errorf("read %s of the caller's status: %w", key, err)
		}

		return n, nil
	}

	return 0, fmt.Errorf("the caller's status has no %s", key)
}

// runRequest hands PID 1 the request on stdin and prints its answer on stdout; any answer printed is a 0, and an empty stdout is a failure to reach PID 1.
func runRequest(stdin io.Reader, stdout io.Writer) int {
	var m supervisor.Message
	if err := supervisor.ReadHeader(stdin, &m); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init: read the request:", err)

		return 1
	}
	reply, err := forward(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "shard-init:", err)

		return 1
	}
	if err := supervisor.WriteMessage(stdout, reply); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init: print the answer:", err)

		return 1
	}

	return 0
}

// forward passes one request to PID 1 and answers what it says; a PID 1 that never bound the socket predates named processes.
func forward(m supervisor.Message) (supervisor.Message, error) {
	conn, err := net.Dial("unix", requestAddr)
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT) {
		return supervisor.Message{Kind: supervisor.KindFailure, ID: m.ID, Error: errOutdated.Error(), Outdated: true}, nil
	}
	if err != nil {
		return supervisor.Message{}, fmt.Errorf("reach PID 1: %w", err)
	}
	defer conn.Close()

	if err := supervisor.WriteMessage(conn, m); err != nil {
		return supervisor.Message{}, fmt.Errorf("send the request to PID 1: %w", err)
	}
	var reply supervisor.Message
	if err := supervisor.ReadHeader(conn, &reply); err != nil {
		return supervisor.Message{}, fmt.Errorf("read the answer of PID 1: %w", err)
	}

	return reply, nil
}
