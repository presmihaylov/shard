package lograte

import (
	"bytes"
	"fmt"
	"log"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *lockedBuffer) lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return strings.Split(strings.TrimSuffix(b.buf.String(), "\n"), "\n")
}

func newLog(t *testing.T) (*Log, *lockedBuffer) {
	t.Helper()

	out := &lockedBuffer{}
	l := New(log.New(out, "", 0), "dns")
	l.tally = 50 * time.Millisecond

	return l, out
}

// awaitCount waits for the line that counts what want's source held back.
func awaitCount(t *testing.T, out *lockedBuffer, want string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if slices.Contains(out.lines(), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no line %q in:\n%s", want, strings.Join(out.lines(), "\n"))
}

func TestAFloodWritesTheBurstAndThenCountsTheRest(t *testing.T) {
	l, out := newLog(t)
	guest := netip.MustParseAddr("10.0.0.2")

	for i := range 1000 {
		l.Printf(guest, "dns: %s: line %d", guest, i)
	}

	written := len(out.lines())
	if written < burst || written > burst+1 {
		t.Fatalf("a flood of 1000 wrote %d lines, want the burst of %d", written, burst)
	}
	awaitCount(t, out, fmt.Sprintf("dns: 10.0.0.2: held back %d lines past the log bound", 1000-written))
}

func TestOneSourcePastItsBoundHoldsNoOtherSourceBack(t *testing.T) {
	l, out := newLog(t)
	flood, quiet := netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("10.0.0.3")

	for range 100 {
		l.Printf(flood, "dns: %s: flood", flood)
	}
	l.Printf(quiet, "dns: %s: one fault", quiet)

	if got := out.lines(); got[len(got)-1] != "dns: 10.0.0.3: one fault" {
		t.Fatalf("the quiet source's line was held back:\n%s", strings.Join(got, "\n"))
	}
}

func TestSourcesPastTheTrackedBoundShareOneBound(t *testing.T) {
	l, out := newLog(t)

	for i := range maxSources {
		l.Printf(netip.AddrFrom4([4]byte{10, 1, byte(i >> 8), byte(i)}), "proxy: tracked")
	}
	for i := range 100 {
		l.Printf(netip.AddrFrom4([4]byte{10, 2, 0, byte(i)}), "proxy: untracked")
	}

	if n := len(l.sources); n != maxSources+1 {
		t.Errorf("%d sources are tracked, want %d and the shared one", n, maxSources)
	}
	untracked := 0
	for _, line := range out.lines() {
		if line == "proxy: untracked" {
			untracked++
		}
	}
	if untracked > burst+1 {
		t.Errorf("100 untracked sources wrote %d lines, want one shared burst of %d", untracked, burst)
	}
	awaitCount(t, out, fmt.Sprintf("dns: other sources: held back %d lines past the log bound", 100-untracked))
}

func TestAnIdleSourceIsForgottenToMakeRoom(t *testing.T) {
	l, _ := newLog(t)

	for i := range maxSources {
		l.Printf(netip.AddrFrom4([4]byte{10, 1, byte(i >> 8), byte(i)}), "proxy: one line")
	}
	// Each source spent one token, so a full bucket is half a second away at 2 a second.
	time.Sleep(time.Second / perSecond)

	l.Printf(netip.MustParseAddr("10.2.0.1"), "proxy: new")
	if _, ok := l.sources[netip.MustParseAddr("10.2.0.1")]; !ok {
		t.Errorf("a new source shares the bound while %d idle ones are tracked", len(l.sources))
	}
}

func TestLoggerChargesEachLineToTheSourceItNames(t *testing.T) {
	l, out := newLog(t)
	logger := l.Logger(func(line string) netip.Addr {
		addr, _, _ := strings.Cut(strings.TrimPrefix(line, "from "), ": ")

		return netip.MustParseAddrPort(addr).Addr()
	})

	for range 100 {
		logger.Printf("from 10.0.0.2:5555: a handshake fault")
	}
	logger.Printf("from 10.0.0.3:5555: a handshake fault")

	got := out.lines()
	if len(got) > burst+2 || got[len(got)-1] != "from 10.0.0.3:5555: a handshake fault" {
		t.Fatalf("the lines are:\n%s", strings.Join(got, "\n"))
	}
}
