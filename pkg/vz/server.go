package vz

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
)

// Machine is the VM the shim holds, so the socket loop has one shape on every platform and a fake in tests.
type Machine interface {
	State() State
	MachineID() string
	Pause() error
	Resume() error
	Save(path string) error
	Stop() error
	Connect(port uint32) (net.Conn, error)
}

// A client that connects and sends nothing within this is dropped, so it cannot keep the shim from exiting.
const handshakeTimeout = 5 * time.Second

// Serve answers on the shim socket until the listener closes. One request per connection.
func Serve(listener net.Listener, machine Machine, logger *log.Logger) error {
	var wg sync.WaitGroup
	defer wg.Wait()

	// The connections still in their handshake, which a closing listener ends rather than waits for.
	var mu sync.Mutex
	pending := map[net.Conn]struct{}{}
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		for conn := range pending {
			if err := conn.Close(); err != nil {
				logger.Printf("shim socket: close a pending connection: %v", err)
			}
		}
	}()

	for {
		conn, err := listener.Accept()
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("accept on the shim socket: %w", err)
		}
		mu.Lock()
		pending[conn] = struct{}{}
		mu.Unlock()

		wg.Go(func() {
			settle := func() {
				mu.Lock()
				defer mu.Unlock()
				delete(pending, conn)
			}
			if err := serveOne(conn, machine, settle); err != nil {
				logger.Printf("shim socket: %v", err)
			}
		})
	}
}

// settled runs once the request frame is in, and again on exit, so a failed handshake leaves nothing behind for shutdown to close.
func serveOne(conn net.Conn, machine Machine, settled func()) error {
	defer conn.Close()
	defer settled()

	var req request
	if err := conn.SetReadDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return fmt.Errorf("bound the handshake: %w", err)
	}
	if err := readFrame(conn, &req); err != nil {
		return err
	}
	settled()
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return fmt.Errorf("clear the handshake deadline: %w", err)
	}

	guest, err := handle(req, machine)
	reply := response{State: machine.State(), PID: os.Getpid(), MachineID: machine.MachineID()}
	if err != nil {
		reply = response{Error: err.Error()}
	}
	if err := writeFrame(conn, reply); err != nil {
		return err
	}
	if guest == nil {
		return nil
	}

	defer guest.Close()

	return splice(conn, guest)
}

// handle runs one verb; only connect hands back a guest stream to splice onto the connection.
func handle(req request, machine Machine) (net.Conn, error) {
	switch req.Verb {
	case "state":
		return nil, nil
	case "pause":
		return nil, machine.Pause()
	case "resume":
		return nil, machine.Resume()
	case "save":
		return nil, machine.Save(req.Path)
	case "stop":
		return nil, machine.Stop()
	case "connect":
		return machine.Connect(req.Port)
	}

	return nil, fmt.Errorf("unknown verb %q", req.Verb)
}

// splice copies both ways until one side ends, then ends the other copier's read.
func splice(a, b net.Conn) error {
	sent := make(chan error, 1)
	go func() { sent <- forward(b, a) }()

	received := forward(a, b)
	if err := a.SetReadDeadline(time.Now()); err != nil {
		return errors.Join(received, fmt.Errorf("end the read of the shim socket: %w", err))
	}

	return errors.Join(received, <-sent)
}

func forward(dst, src net.Conn) error {
	_, err := io.Copy(dst, src)
	if quiet(err) {
		return nil
	}

	return err
}

// quiet reports the ends that are how a spliced connection stops, rather than a failure to report.
func quiet(err error) bool {
	return err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET)
}
