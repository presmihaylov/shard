package linkfwd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/network"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/sandboxstate"
	"github.com/presmihaylov/shard/services/sshd"
)

var (
	agentAddr = netip.MustParseAddr("10.87.0.2")
	dbLink    = netip.MustParseAddr("10.86.0.1")
	cacheLink = netip.MustParseAddr("10.86.0.2")
)

type fakeRecords struct {
	mu        sync.Mutex
	sandboxes map[string]models.Sandbox
	broken    map[string]error
}

func (r *fakeRecords) Get(id string) (models.Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.broken[id]; err != nil {
		return models.Sandbox{}, err
	}
	sb, found := r.sandboxes[id]
	if !found {
		return models.Sandbox{}, models.NotFound(sandboxstate.ErrNotFound, "sandbox "+id+" not found")
	}

	return sb, nil
}

func (r *fakeRecords) List() ([]models.Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []models.Sandbox
	for _, sb := range r.sandboxes {
		out = append(out, sb)
	}
	slices.SortFunc(out, func(a, b models.Sandbox) int { return strings.Compare(a.ID, b.ID) })

	return out, nil
}

func (r *fakeRecords) put(sb models.Sandbox) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sandboxes[sb.ID] = sb
}

func (r *fakeRecords) remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sandboxes, id)
}

// fakeSandboxes hands each dial one end of a pipe and keeps the other, as the target's listener.
type fakeSandboxes struct {
	mu      sync.Mutex
	stopped map[string]bool
	dialed  []string
	peers   []net.Conn
	// hold, when set, keeps a dial in flight until it is closed.
	hold chan struct{}
}

func (s *fakeSandboxes) refusal(ref string) error {
	if s.stopped[ref] {
		return &sandbox.StateError{Sandbox: ref, State: models.StateStopped, Code: models.CodeSandboxNotRunning}
	}

	return nil
}

func (s *fakeSandboxes) ReadyForSSH(_ context.Context, ref string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return ref, s.refusal(ref)
}

func (s *fakeSandboxes) DialPort(ctx context.Context, ref string, port uint16) (net.Conn, error) {
	s.mu.Lock()
	hold := s.hold
	s.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refusal(ref); err != nil {
		return nil, err
	}
	s.dialed = append(s.dialed, fmt.Sprintf("%s:%d", ref, port))
	client, server := net.Pipe()
	s.peers = append(s.peers, server)

	return client, nil
}

func (s *fakeSandboxes) peer(i int) net.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.peers[i]
}

type fakeSSH struct {
	served chan sshd.Options
	// left, when set, keeps each session until its connection ends and then says so.
	left chan struct{}
}

func (s fakeSSH) Serve(_ context.Context, conn net.Conn, opts sshd.Options) error {
	s.served <- opts
	if s.left == nil {
		return conn.Close()
	}

	_, err := io.Copy(io.Discard, conn)
	s.left <- struct{}{}

	return errors.Join(err, conn.Close())
}

type harness struct {
	records   *fakeRecords
	sandboxes *fakeSandboxes
	ssh       fakeSSH
	reports   chan string
	f         *Forwarder
}

func agent(links ...models.Link) models.Sandbox {
	return models.Sandbox{ID: "agent", State: models.StateRunning, Address: netip.PrefixFrom(agentAddr, 16), Links: links}
}

func link(target string, addr netip.Addr) models.Link {
	return models.Link{Target: target, TargetID: target + "-id", Address: addr}
}

func newHarness(t *testing.T, sandboxes ...models.Sandbox) *harness {
	t.Helper()
	h := &harness{
		records:   &fakeRecords{sandboxes: map[string]models.Sandbox{}},
		sandboxes: &fakeSandboxes{stopped: map[string]bool{}},
		ssh:       fakeSSH{served: make(chan sshd.Options, 4)},
		reports:   make(chan string, 16),
	}
	for _, sb := range sandboxes {
		h.records.put(sb)
	}
	h.f = New(h.records, h.sandboxes, h.ssh, func(line string) { h.reports <- line })

	return h
}

func (h *harness) flows() int {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()

	return len(h.f.flows)
}

// ended says the target's end of a pipe saw the forwarder's end close; a pipe refuses a deadline once either end has.
func ended(peer net.Conn) bool {
	if err := peer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return errors.Is(err, io.ErrClosedPipe)
	}
	_, err := peer.Read(make([]byte, 1))

	return errors.Is(err, io.EOF)
}

func TestDialReachesTheTargetPortOfTheLink(t *testing.T) {
	h := newHarness(t, agent(link("db", dbLink)))

	conn, err := h.f.Dial(t.Context(), agentAddr, netip.AddrPortFrom(dbLink, 5432))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if !slices.Equal(h.sandboxes.dialed, []string{"db-id:5432"}) {
		t.Errorf("dialed %v, want the target's 5432", h.sandboxes.dialed)
	}
	go func() { _, _ = conn.Write([]byte("hi")) }()
	got := make([]byte, 2)
	if _, err := io.ReadFull(h.sandboxes.peer(0), got); err != nil || string(got) != "hi" {
		t.Errorf("the target read %q, %v", got, err)
	}
}

func TestDialServesPort22ThroughTheSSHServer(t *testing.T) {
	h := newHarness(t, agent(link("db", dbLink)))

	conn, err := h.f.Dial(t.Context(), agentAddr, netip.AddrPortFrom(dbLink, SSHPort))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	select {
	case opts := <-h.ssh.served:
		if opts.Sandbox != "db-id" || opts.Explain == nil {
			t.Errorf("ssh served %+v, want the target with an explain", opts)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the ssh server got no connection")
	}
	if len(h.sandboxes.dialed) != 0 {
		t.Errorf("port 22 dialed the target's own loopback: %v", h.sandboxes.dialed)
	}
}

func TestDialRefusesWhatNoLinkReaches(t *testing.T) {
	failed := agent(link("db", dbLink))
	failed.ID, failed.State = "failed", models.StateFailed
	failed.Address = netip.PrefixFrom(netip.MustParseAddr("10.87.0.9"), 16)

	for name, dst := range map[string]struct {
		source netip.Addr
		dst    netip.Addr
	}{
		"a source no sandbox holds": {netip.MustParseAddr("10.87.0.5"), dbLink},
		"a link the source lacks":   {agentAddr, cacheLink},
		"a source that failed":      {netip.MustParseAddr("10.87.0.9"), dbLink},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, agent(link("db", dbLink)), failed)

			_, err := h.f.Dial(t.Context(), dst.source, netip.AddrPortFrom(dst.dst, 5432))
			if !errors.Is(err, ErrNoLink) || !refused(err) {
				t.Errorf("Dial = %v, want a refusal for no link", err)
			}
			if len(h.sandboxes.dialed) != 0 || h.flows() != 0 {
				t.Errorf("a refused flow dialed %v or stayed tracked", h.sandboxes.dialed)
			}
		})
	}
}

func TestDialRefusesAStoppedTargetAndKeepsTheLink(t *testing.T) {
	for _, port := range []uint16{5432, SSHPort} {
		t.Run(fmt.Sprint(port), func(t *testing.T) {
			h := newHarness(t, agent(link("db", dbLink)))
			h.sandboxes.stopped["db-id"] = true

			_, err := h.f.Dial(t.Context(), agentAddr, netip.AddrPortFrom(dbLink, port))
			if _, stopped := errors.AsType[*sandbox.StateError](err); !stopped || !refused(err) {
				t.Errorf("Dial = %v, want the target's state refusal", err)
			}
			if h.flows() != 0 {
				t.Error("a refused flow stayed tracked")
			}

			delete(h.sandboxes.stopped, "db-id")
			conn, err := h.f.Dial(t.Context(), agentAddr, netip.AddrPortFrom(dbLink, port))
			if err != nil {
				t.Fatalf("the link did not outlive the stop: %v", err)
			}
			_ = conn.Close()
		})
	}
}

func TestChangedEndsTheFlowsToAnUnlinkedTarget(t *testing.T) {
	other := models.Sandbox{ID: "other", State: models.StateRunning, Address: netip.PrefixFrom(netip.MustParseAddr("10.87.0.3"), 16), Links: []models.Link{link("db", dbLink)}}
	h := newHarness(t, agent(link("db", dbLink), link("cache", cacheLink)), other)

	var conns []net.Conn
	for _, dial := range []struct {
		source netip.Addr
		dst    netip.Addr
	}{{agentAddr, dbLink}, {agentAddr, cacheLink}, {other.Address.Addr(), dbLink}} {
		conn, err := h.f.Dial(t.Context(), dial.source, netip.AddrPortFrom(dial.dst, 5432))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		conns = append(conns, conn)
	}

	h.records.put(agent(link("cache", cacheLink)))
	if err := h.f.Changed(t.Context(), "agent"); err != nil {
		t.Fatal(err)
	}

	if !ended(h.sandboxes.peer(0)) {
		t.Error("the flow to the unlinked target is still open")
	}
	for _, conn := range conns[1:] {
		if err := conn.SetReadDeadline(time.Time{}); err != nil {
			t.Errorf("a flow the change did not touch ended: %v", err)
		}
	}
	if h.flows() != 2 {
		t.Errorf("%d flows tracked, want the two still linked", h.flows())
	}
}

func TestChangedEndsEveryFlowOnceTheSourceIsGone(t *testing.T) {
	h := newHarness(t, agent(link("db", dbLink)))
	conn, err := h.f.Dial(t.Context(), agentAddr, netip.AddrPortFrom(dbLink, 5432))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	h.records.remove("agent")
	if err := h.f.Changed(t.Context(), "agent"); err != nil {
		t.Errorf("Changed = %v, want no error for a removed sandbox", err)
	}
	if !ended(h.sandboxes.peer(0)) || h.flows() != 0 {
		t.Error("a flow from a removed sandbox stayed open")
	}
}

func TestChangedEndsTheFlowsOfARecordItCannotRead(t *testing.T) {
	h := newHarness(t, agent(link("db", dbLink)))
	conn, err := h.f.Dial(t.Context(), agentAddr, netip.AddrPortFrom(dbLink, 5432))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	cause := errors.New("record unreadable")
	h.records.broken = map[string]error{"agent": cause}
	if err := h.f.Changed(t.Context(), "agent"); !errors.Is(err, cause) {
		t.Errorf("Changed = %v, want the record's error", err)
	}
	if !ended(h.sandboxes.peer(0)) {
		t.Error("a flow whose link could not be proved stayed open")
	}
}

func TestChangedEndsADialInFlight(t *testing.T) {
	h := newHarness(t, agent(link("db", dbLink)))
	h.sandboxes.hold = make(chan struct{})

	dialed := make(chan error, 1)
	go func() {
		_, err := h.f.Dial(context.Background(), agentAddr, netip.AddrPortFrom(dbLink, 5432))
		dialed <- err
	}()
	for h.flows() == 0 {
		time.Sleep(time.Millisecond)
	}

	h.records.put(agent())
	if err := h.f.Changed(t.Context(), "agent"); err != nil {
		t.Fatal(err)
	}
	close(h.sandboxes.hold)

	if err := <-dialed; !errors.Is(err, ErrNoLink) {
		t.Errorf("Dial = %v, want the link gone", err)
	}
	if !ended(h.sandboxes.peer(0)) || h.flows() != 0 {
		t.Error("the dial the unlink raced kept its connection")
	}
}

func TestCloseForgetsTheFlow(t *testing.T) {
	h := newHarness(t, agent(link("db", dbLink)))
	conn, err := h.f.Dial(t.Context(), agentAddr, netip.AddrPortFrom(dbLink, 5432))
	if err != nil {
		t.Fatal(err)
	}

	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if h.flows() != 0 {
		t.Error("a closed flow stayed tracked")
	}
}

// The splice half-closes a target that can, so the wrapper must not hide CloseWrite, nor claim one a pipe lacks.
func TestTheFlowKeepsTheTargetsHalfClose(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	tcp, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	pipe, _ := net.Pipe()

	f := New(&fakeRecords{}, &fakeSandboxes{}, fakeSSH{}, func(string) {})
	if _, ok := wrap(f, &flow{}, tcp).(interface{ CloseWrite() error }); !ok {
		t.Error("a TCP target lost its CloseWrite")
	}
	if _, ok := wrap(f, &flow{}, pipe).(interface{ CloseWrite() error }); ok {
		t.Error("a pipe target gained a CloseWrite")
	}
}

func TestRoutesListsEachSourceWithItsLinks(t *testing.T) {
	later := models.Sandbox{ID: "a-later", State: models.StateStopped, Address: netip.PrefixFrom(netip.MustParseAddr("10.87.0.4"), 16), Links: []models.Link{link("db", dbLink)}}
	records := &fakeRecords{sandboxes: map[string]models.Sandbox{}}
	for _, sb := range []models.Sandbox{
		agent(link("db", dbLink), link("cache", cacheLink)),
		later,
		{ID: "lonely", State: models.StateRunning, Address: netip.PrefixFrom(netip.MustParseAddr("10.87.0.5"), 16)},
		{ID: "failed", State: models.StateFailed, Address: netip.PrefixFrom(netip.MustParseAddr("10.87.0.6"), 16), Links: []models.Link{link("db", dbLink)}},
		{ID: "unplaced", State: models.StatePending, Links: []models.Link{link("db", dbLink)}},
	} {
		records.put(sb)
	}

	got, err := Routes{Records: records}.Linked(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	want := []network.Linked{{Source: agentAddr, Targets: []netip.Addr{dbLink, cacheLink}}, {Source: later.Address.Addr(), Targets: []netip.Addr{dbLink}}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Linked = %v, want %v", got, want)
	}
}

// The stack path resets the guest itself, so Intercept keeps the ordinary refusal out of the log and reports the rest.
func TestInterceptReportsWhatIsNoOrdinaryRefusal(t *testing.T) {
	h := newHarness(t, agent(link("db", dbLink)))

	if _, err := h.f.Intercept(t.Context(), agentAddr, netip.AddrPortFrom(cacheLink, 5432)); !errors.Is(err, ErrNoLink) {
		t.Fatalf("Intercept = %v, want a refusal for no link", err)
	}
	select {
	case line := <-h.reports:
		t.Errorf("an ordinary refusal reached the log: %s", line)
	default:
	}

	cause := errors.New("record unreadable")
	h.records.broken = map[string]error{"agent": cause}
	if _, err := h.f.Intercept(t.Context(), agentAddr, netip.AddrPortFrom(dbLink, 5432)); !errors.Is(err, cause) {
		t.Fatalf("Intercept = %v, want the record's error", err)
	}
	select {
	case line := <-h.reports:
		if !strings.Contains(line, cause.Error()) {
			t.Errorf("reported %q, want the record's error", line)
		}
	default:
		t.Error("a broken record never reached the log")
	}
}

// The listener path: the guest's address names the source, conntrack names the link, and a refusal is a reset.
func TestServeCarriesAGuestFlowAndResetsARefusedOne(t *testing.T) {
	loopback := netip.MustParseAddr("127.0.0.1")
	source := agent(link("db", dbLink))
	source.Address = netip.PrefixFrom(loopback, 8)
	h := newHarness(t, source)
	var dst atomic.Value
	dst.Store(netip.AddrPortFrom(dbLink, 5432))
	h.f.original = func(net.Conn) (netip.AddrPort, error) { return dst.Load().(netip.AddrPort), nil }

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- h.f.Serve(ctx, l) }()

	guest, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer guest.Close()
	if _, err := guest.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	for h.flows() == 0 {
		time.Sleep(time.Millisecond)
	}
	got := make([]byte, 2)
	if _, err := io.ReadFull(h.sandboxes.peer(0), got); err != nil || string(got) != "hi" {
		t.Errorf("the target read %q, %v", got, err)
	}

	dst.Store(netip.AddrPortFrom(cacheLink, 5432))
	refused, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer refused.Close()
	if err := refused.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := refused.Read(make([]byte, 1)); !errors.Is(err, syscall.ECONNRESET) {
		t.Errorf("a flow to no link read %v, want a reset", err)
	}
	select {
	case line := <-h.reports:
		t.Errorf("an ordinary refusal reached the log: %s", line)
	default:
	}

	cancel()
	if err := <-served; err != nil {
		t.Errorf("Serve = %v, want nil once ctx ended", err)
	}
}

// Shutdown ends the flows a guest still holds open, a port 22 session among them, before Serve returns.
func TestServeEndsItsFlowsAndWaitsForThemOnceCtxEnds(t *testing.T) {
	loopback := netip.MustParseAddr("127.0.0.1")
	source := agent(link("db", dbLink))
	source.Address = netip.PrefixFrom(loopback, 8)
	h := newHarness(t, source)
	h.ssh.left = make(chan struct{}, 1)
	h.f.ssh = h.ssh
	var dst atomic.Value
	dst.Store(netip.AddrPortFrom(dbLink, 5432))
	h.f.original = func(net.Conn) (netip.AddrPort, error) { return dst.Load().(netip.AddrPort), nil }

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- h.f.Serve(ctx, l) }()

	guest, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer guest.Close()
	for h.flows() == 0 {
		time.Sleep(time.Millisecond)
	}
	dst.Store(netip.AddrPortFrom(dbLink, SSHPort))
	shell, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer shell.Close()
	<-h.ssh.served
	for h.flows() < 2 {
		time.Sleep(time.Millisecond)
	}

	cancel()
	if err := <-served; err != nil {
		t.Errorf("Serve = %v, want nil once ctx ended", err)
	}
	if !ended(h.sandboxes.peer(0)) {
		t.Error("the target's end stayed open after Serve returned")
	}
	select {
	case <-h.ssh.left:
	default:
		t.Error("Serve returned before the ssh session ended")
	}
	if n := h.flows(); n != 0 {
		t.Errorf("%d flows tracked after Serve returned, want none", n)
	}
	if err := guest.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := guest.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("the guest read %v, want EOF once Serve ended its flow", err)
	}
}
