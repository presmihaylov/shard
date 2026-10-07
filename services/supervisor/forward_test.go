package supervisor_test

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/services/supervisor"
)

// guestForward serves one forward on the guest end of a pipe: it reads the header, answers refusal or not, and hands back the wrapped end.
func guestForward(t *testing.T, conn net.Conn, refusal string) <-chan *supervisor.ForwardGuest {
	t.Helper()
	served := make(chan *supervisor.ForwardGuest, 1)
	go func() {
		var header supervisor.ForwardHeader
		if err := supervisor.ReadHeader(conn, &header); err != nil {
			t.Errorf("read the forward header: %v", err)
		}
		if header.Port != 8100 {
			t.Errorf("header names port %d, want 8100", header.Port)
		}
		if err := supervisor.WriteMessage(conn, supervisor.ForwardReply{Error: refusal}); err != nil {
			t.Errorf("answer the forward: %v", err)
		}
		served <- supervisor.NewForwardGuest(conn)
	}()

	return served
}

func TestOpenForwardCarriesTheHostsHalfCloseAsAFrame(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	served := guestForward(t, guest, "")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	forward, err := supervisor.OpenForward(ctx, host, 8100)
	if err != nil {
		t.Fatalf("open the forward: %v", err)
	}
	end := <-served
	go func() {
		if _, err := forward.Write([]byte("ping")); err != nil {
			t.Errorf("write: %v", err)
		}
		if err := forward.CloseWrite(); err != nil {
			t.Errorf("close write: %v", err)
		}
	}()
	got, err := io.ReadAll(end)
	if err != nil || string(got) != "ping" {
		t.Fatalf("guest read %q, %v; want ping then EOF", got, err)
	}

	go func() {
		if _, err := end.Write([]byte("pong")); err != nil {
			t.Errorf("guest write: %v", err)
		}
		if err := end.Close(); err != nil {
			t.Errorf("guest close: %v", err)
		}
	}()
	got, err = io.ReadAll(forward)
	if err != nil || string(got) != "pong" {
		t.Fatalf("host read %q, %v; want pong as plain bytes", got, err)
	}
}

func TestOpenForwardNamesTheGuestsRefusal(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	guestForward(t, guest, "connection refused")

	_, err := supervisor.OpenForward(t.Context(), host, 8100)
	if err == nil || !strings.Contains(err.Error(), "port 8100") || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("open gave %v, want the refusal naming port 8100", err)
	}
}

// A host gone without its half close is a reset to the guest, so its splice closes both ends instead of waiting.
func TestForwardGuestReadsAHostThatVanishedAsAReset(t *testing.T) {
	host, guest := net.Pipe()
	defer guest.Close()
	end := supervisor.NewForwardGuest(guest)
	if err := host.Close(); err != nil {
		t.Fatalf("close the host: %v", err)
	}

	if _, err := end.Read(make([]byte, 1)); !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("read gave %v, want ECONNRESET", err)
	}
}
