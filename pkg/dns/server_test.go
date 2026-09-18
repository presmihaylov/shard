package dns

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

var answerAddr = [4]byte{93, 184, 216, 34}

// fakeDirector allows the names it lists, fails when told to, and remembers what it was asked.
type fakeDirector struct {
	allowed []string
	err     error

	mu    sync.Mutex
	asked []Question
}

func (d *fakeDirector) Resolve(_ context.Context, q Question) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.asked = append(d.asked, q)
	if d.err != nil {
		return false, d.err
	}

	return slices.Contains(d.allowed, q.Name), nil
}

func (d *fakeDirector) questions() []Question {
	d.mu.Lock()
	defer d.mu.Unlock()

	return slices.Clone(d.asked)
}

// syncBuffer is a log sink the handlers write from their own goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// fakeUpstream answers every question with one A record, over udp and tcp on one loopback port, and counts them.
type fakeUpstream struct {
	addr  netip.AddrPort
	asked atomic.Int32
}

func newUpstream(t *testing.T) *fakeUpstream {
	t.Helper()

	// udp and tcp ports are separate spaces, so the tcp bind on the udp port is expected to work.
	var udp net.PacketConn
	var tcp net.Listener
	for attempt := 0; attempt < 10 && tcp == nil; attempt++ {
		var err error

		udp, err = net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}

		tcp, err = net.Listen("tcp", udp.LocalAddr().String())
		if err != nil {
			if err := udp.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if tcp == nil {
		t.Fatal("no loopback port was free on both udp and tcp")
	}

	u := &fakeUpstream{addr: netip.MustParseAddrPort(udp.LocalAddr().String())}

	go func() {
		buf := make([]byte, maxMessage)
		for {
			n, from, err := udp.ReadFrom(buf)
			if err != nil {
				return
			}
			if _, err := udp.WriteTo(u.answer(t, buf[:n]), from); err != nil {
				return
			}
		}
	}()

	go func() {
		for {
			conn, err := tcp.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()

				msg, err := readFramed(conn)
				if err != nil {
					return
				}
				if _, err := conn.Write(framed(u.answer(t, msg))); err != nil {
					return
				}
			}()
		}
	}()

	t.Cleanup(func() {
		if err := errors.Join(udp.Close(), tcp.Close()); err != nil {
			t.Error(err)
		}
	})

	return u
}

func (u *fakeUpstream) answer(t *testing.T, msg []byte) []byte {
	t.Helper()

	u.asked.Add(1)

	var p dnsmessage.Parser
	header, err := p.Start(msg)
	if err != nil {
		t.Errorf("the upstream got a message it cannot parse: %v", err)

		return nil
	}
	q, err := p.Question()
	if err != nil {
		t.Errorf("the upstream got a message with no question: %v", err)

		return nil
	}

	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: header.ID, Response: true, RecursionAvailable: true})
	if err := errors.Join(
		b.StartQuestions(),
		b.Question(q),
		b.StartAnswers(),
		b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60}, dnsmessage.AResource{A: answerAddr}),
	); err != nil {
		t.Errorf("the upstream cannot build its answer: %v", err)

		return nil
	}

	out, err := b.Finish()
	if err != nil {
		t.Errorf("the upstream cannot finish its answer: %v", err)

		return nil
	}

	return out
}

// resolver is a Server under test, serving on two loopback ports of its own until the test ends.
type resolver struct {
	udp, tcp netip.AddrPort
	log      *syncBuffer
}

func serve(t *testing.T, director Director, upstreams ...netip.AddrPort) resolver {
	t.Helper()

	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	return serveOn(t, director, tcp, upstreams...)
}

// serveOn is serve over a tcp listener the test hands in, so the connections can come from any source it names.
func serveOn(t *testing.T, director Director, tcp net.Listener, upstreams ...netip.AddrPort) resolver {
	t.Helper()

	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	sink := &syncBuffer{}
	server, err := New(Config{Address: netip.MustParseAddr("127.0.0.1"), Upstreams: upstreams, Director: director, Log: log.New(sink, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	// A dead upstream must fail within the test, not within the five seconds a guest is given.
	server.timeout = 200 * time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, udp, tcp) }()

	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})

	return resolver{
		udp: netip.MustParseAddrPort(udp.LocalAddr().String()),
		tcp: netip.MustParseAddrPort(tcp.Addr().String()),
		log: sink,
	}
}

// pipeListener hands the resolver the connections a test dials, each from the source address the test names.
type pipeListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.done) })

	return nil
}

func (l *pipeListener) Addr() net.Addr {
	return net.TCPAddrFromAddrPort(netip.MustParseAddrPort("10.87.0.1:53"))
}

// dial gives the test the guest end of a connection the resolver has accepted as coming from source.
func (l *pipeListener) dial(t *testing.T, source string) net.Conn {
	t.Helper()

	guest, server := net.Pipe()
	select {
	case l.conns <- sourced{Conn: server, remote: netip.MustParseAddrPort(source)}:
	case <-time.After(2 * time.Second):
		t.Fatal("the resolver did not accept a connection")
	}

	return guest
}

// sourced is a connection whose remote address is what the test says, so one test can speak as several sandboxes.
type sourced struct {
	net.Conn
	remote netip.AddrPort
}

func (c sourced) RemoteAddr() net.Addr { return net.TCPAddrFromAddrPort(c.remote) }

// ask sends one framed question on a tcp connection and returns the answer, or fails when none comes within the wait.
func ask(t *testing.T, conn net.Conn, msg []byte, wait time.Duration) []byte {
	t.Helper()

	if err := conn.SetDeadline(time.Now().Add(wait)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(framed(msg)); err != nil {
		t.Fatalf("write the question: %v", err)
	}

	answer, err := readFramed(conn)
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}

	return answer
}

// stallingDirector never finishes judging one source's questions until released, so its handlers pile up and stay.
type stallingDirector struct {
	stalled netip.Addr
	release chan struct{}
}

func (d *stallingDirector) Resolve(ctx context.Context, q Question) (bool, error) {
	if q.Source != d.stalled {
		return true, nil
	}

	select {
	case <-d.release:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func question(t *testing.T, id uint16, name string) []byte {
	t.Helper()

	return message(t, dnsmessage.Header{ID: id, RecursionDesired: true}, name)
}

func message(t *testing.T, header dnsmessage.Header, names ...string) []byte {
	t.Helper()

	b := dnsmessage.NewBuilder(nil, header)
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if err := b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}); err != nil {
			t.Fatal(err)
		}
	}

	out, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}

	return out
}

// askUDP sends one question and returns the answer, or nil when none comes within the wait.
func askUDP(t *testing.T, to netip.AddrPort, msg []byte, wait time.Duration) []byte {
	t.Helper()

	conn, err := net.Dial("udp", to.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := conn.Write(msg); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(wait)); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, maxMessage)
	n, err := conn.Read(buf)
	if errors.Is(err, io.EOF) || (err != nil && strings.Contains(err.Error(), "timeout")) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}

	return buf[:n]
}

// parsed is the shape of an answer a test asserts on.
type parsed struct {
	header    dnsmessage.Header
	questions []dnsmessage.Question
	answers   []netip.Addr
}

func parse(t *testing.T, msg []byte) parsed {
	t.Helper()

	var p dnsmessage.Parser
	header, err := p.Start(msg)
	if err != nil {
		t.Fatalf("parse the answer: %v", err)
	}
	questions, err := p.AllQuestions()
	if err != nil {
		t.Fatalf("parse the questions: %v", err)
	}
	resources, err := p.AllAnswers()
	if err != nil {
		t.Fatalf("parse the answers: %v", err)
	}

	out := parsed{header: header, questions: questions}
	for _, r := range resources {
		a, ok := r.Body.(*dnsmessage.AResource)
		if !ok {
			t.Fatalf("an answer that is not an A record: %v", r)
		}
		out.answers = append(out.answers, netip.AddrFrom4(a.A))
	}

	return out
}

func TestAnAllowedQuestionIsAnsweredByTheUpstream(t *testing.T) {
	upstream := newUpstream(t)
	director := &fakeDirector{allowed: []string{"api.example.com"}}
	r := serve(t, director, upstream.addr)

	got := parse(t, askUDP(t, r.udp, question(t, 41, "API.Example.COM."), 2*time.Second))

	if got.header.ID != 41 || !got.header.Response || got.header.RCode != dnsmessage.RCodeSuccess {
		t.Errorf("header = %+v", got.header)
	}
	if len(got.answers) != 1 || got.answers[0] != netip.AddrFrom4(answerAddr) {
		t.Errorf("answers = %v, want the upstream's %v", got.answers, netip.AddrFrom4(answerAddr))
	}
	// The director judges the canonical name, the way the proxy judges a host, and knows who asked.
	want := []Question{{Source: netip.MustParseAddr("127.0.0.1"), Name: "api.example.com"}}
	if asked := director.questions(); !slices.Equal(asked, want) {
		t.Errorf("the director was asked %v, want %v", asked, want)
	}
}

func TestADeniedQuestionIsRefusedUnresolved(t *testing.T) {
	upstream := newUpstream(t)
	r := serve(t, &fakeDirector{allowed: []string{"api.example.com"}}, upstream.addr)

	got := parse(t, askUDP(t, r.udp, question(t, 7, "evil.example.net."), 2*time.Second))

	if got.header.ID != 7 || !got.header.Response || got.header.RCode != dnsmessage.RCodeNameError {
		t.Errorf("header = %+v, want NXDOMAIN", got.header)
	}
	// A stub resolver drops an answer that does not echo its question, so the refusal must carry it.
	if len(got.questions) != 1 || got.questions[0].Name.String() != "evil.example.net." {
		t.Errorf("questions = %v, want the one asked", got.questions)
	}
	if len(got.answers) != 0 {
		t.Errorf("answers = %v, want none", got.answers)
	}
	if n := upstream.asked.Load(); n != 0 {
		t.Errorf("the upstream was asked %d times for a denied name", n)
	}
}

func TestTCPCarriesTheSameAnswers(t *testing.T) {
	upstream := newUpstream(t)
	r := serve(t, &fakeDirector{allowed: []string{"api.example.com"}}, upstream.addr)

	conn, err := net.Dial("tcp", r.tcp.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// One connection carries questions in turn, the way a stub that fell back to tcp sends them.
	for _, tc := range []struct {
		name  string
		rcode dnsmessage.RCode
	}{
		{"api.example.com.", dnsmessage.RCodeSuccess},
		{"evil.example.net.", dnsmessage.RCodeNameError},
	} {
		if _, err := conn.Write(framed(question(t, 9, tc.name))); err != nil {
			t.Fatal(err)
		}

		answer, err := readFramed(conn)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}

		got := parse(t, answer)
		if got.header.ID != 9 || got.header.RCode != tc.rcode {
			t.Errorf("%s: header = %+v, want rcode %v", tc.name, got.header, tc.rcode)
		}
	}
	if n := upstream.asked.Load(); n != 1 {
		t.Errorf("the upstream was asked %d times, want once", n)
	}
}

func TestADirectorFaultIsAServerFailure(t *testing.T) {
	upstream := newUpstream(t)
	r := serve(t, &fakeDirector{err: errors.New("the log is full")}, upstream.addr)

	got := parse(t, askUDP(t, r.udp, question(t, 3, "api.example.com."), 2*time.Second))

	if got.header.RCode != dnsmessage.RCodeServerFailure {
		t.Errorf("header = %+v, want SERVFAIL", got.header)
	}
	if n := upstream.asked.Load(); n != 0 {
		t.Errorf("the upstream was asked %d times for a question the director could not judge", n)
	}
	// The fault is logged, and the name is not: the egress log is where a question is recorded.
	if logged := r.log.String(); !strings.Contains(logged, "the log is full") || strings.Contains(logged, "api.example.com") {
		t.Errorf("the log says %q", logged)
	}
}

func TestADeadUpstreamIsAServerFailure(t *testing.T) {
	// Port 1 on loopback is held by nothing, so the exchange fails outright or times out.
	r := serve(t, &fakeDirector{allowed: []string{"api.example.com"}}, netip.MustParseAddrPort("127.0.0.1:1"))

	got := parse(t, askUDP(t, r.udp, question(t, 5, "api.example.com."), 3*time.Second))

	if got.header.ID != 5 || got.header.RCode != dnsmessage.RCodeServerFailure {
		t.Errorf("header = %+v, want SERVFAIL", got.header)
	}
	if logged := r.log.String(); !strings.Contains(logged, "no upstream answered") {
		t.Errorf("the log says %q", logged)
	}
}

func TestAMessageThatIsNoQuestionGetsNoAnswer(t *testing.T) {
	upstream := newUpstream(t)
	director := &fakeDirector{allowed: []string{"api.example.com"}}
	r := serve(t, director, upstream.addr)

	// An answer sent at the resolver would, if answered, make two servers answer each other forever.
	if got := askUDP(t, r.udp, message(t, dnsmessage.Header{ID: 1, Response: true}, "api.example.com."), 300*time.Millisecond); got != nil {
		t.Errorf("a response was answered with %v", parse(t, got))
	}

	// A message with two questions has no one name to judge, so it is a format error and never judged.
	got := parse(t, askUDP(t, r.udp, message(t, dnsmessage.Header{ID: 2}, "api.example.com.", "evil.example.net."), 2*time.Second))
	if got.header.RCode != dnsmessage.RCodeFormatError {
		t.Errorf("header = %+v, want FORMERR", got.header)
	}

	// An unknown opcode is not implemented, and never judged either.
	got = parse(t, askUDP(t, r.udp, message(t, dnsmessage.Header{ID: 3, OpCode: 2}, "api.example.com."), 2*time.Second))
	if got.header.RCode != dnsmessage.RCodeNotImplemented {
		t.Errorf("header = %+v, want NOTIMP", got.header)
	}

	if asked := director.questions(); len(asked) != 0 {
		t.Errorf("the director was asked %v", asked)
	}
	if n := upstream.asked.Load(); n != 0 {
		t.Errorf("the upstream was asked %d times", n)
	}
}

// One sandbox that opens more connections than the whole pool holds, each with a question that never finishes, must
// leave its siblings answered: the per-source bound admits maxPerSource of them and closes the rest at accept.
func TestOneSourceCannotStarveItsSiblings(t *testing.T) {
	upstream := newUpstream(t)
	ln := newPipeListener()
	// The stalled source is loopback, so its udp questions from the test count against the same bound as its tcp ones.
	director := &stallingDirector{stalled: netip.MustParseAddr("127.0.0.1"), release: make(chan struct{})}
	r := serveOn(t, director, ln, upstream.addr)

	var held []net.Conn
	refused := 0
	for i := range maxInflight + 1 {
		conn := ln.dial(t, "127.0.0.1:4000")
		defer conn.Close()

		// A pipe write returns once the resolver read the question, or fails once it closed the connection instead.
		if _, err := conn.Write(framed(question(t, uint16(i), "api.example.com."))); err != nil {
			refused++

			continue
		}
		held = append(held, conn)
	}
	if len(held) != maxPerSource || refused != maxInflight+1-maxPerSource {
		t.Fatalf("the source holds %d connections and was refused %d, want %d and %d", len(held), refused, maxPerSource, maxInflight+1-maxPerSource)
	}

	if got := askUDP(t, r.udp, question(t, 300, "api.example.com."), 300*time.Millisecond); got != nil {
		t.Errorf("a udp question from the source at its bound was answered with %+v, want it dropped", parse(t, got))
	}

	sibling := ln.dial(t, "10.87.0.3:4000")
	defer sibling.Close()
	if got := parse(t, ask(t, sibling, question(t, 301, "api.example.com."), 2*time.Second)); got.header.ID != 301 || got.header.RCode != dnsmessage.RCodeSuccess {
		t.Errorf("the sibling's tcp question got %+v, want an answer", got.header)
	}

	close(director.release)
	for i, conn := range held {
		if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatal(err)
		}
		answer, err := readFramed(conn)
		if err != nil {
			t.Fatalf("held connection %d: %v", i, err)
		}
		if got := parse(t, answer); got.header.RCode != dnsmessage.RCodeSuccess {
			t.Errorf("held connection %d got %+v, want an answer once released", i, got.header)
		}
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}

	// The source's bound frees with its connections, so the same source resolves again.
	if got := parse(t, askUDP(t, r.udp, question(t, 302, "api.example.com."), 2*time.Second)); got.header.RCode != dnsmessage.RCodeSuccess {
		t.Errorf("a udp question after the release got %+v, want an answer", got.header)
	}
}

// Enough idle connections to fill the whole pool, from enough sources to get past the per-source bound, must hold no
// in-flight slot: a slot is taken per question, so one more source is answered at once over tcp and udp.
func TestIdleConnectionsHoldNoSlot(t *testing.T) {
	upstream := newUpstream(t)
	ln := newPipeListener()
	r := serveOn(t, &fakeDirector{allowed: []string{"api.example.com"}}, ln, upstream.addr)

	for source := range maxInflight / maxPerSource {
		for range maxPerSource {
			conn := ln.dial(t, netip.AddrPortFrom(netip.AddrFrom4([4]byte{10, 87, 1, byte(source + 2)}), 4000).String())
			defer conn.Close()
		}
	}

	late := ln.dial(t, "10.87.2.2:4000")
	defer late.Close()
	if got := parse(t, ask(t, late, question(t, 400, "api.example.com."), 2*time.Second)); got.header.RCode != dnsmessage.RCodeSuccess {
		t.Errorf("the tcp question behind %d idle connections got %+v, want an answer", maxInflight, got.header)
	}
	if got := parse(t, askUDP(t, r.udp, question(t, 401, "api.example.com."), 2*time.Second)); got.header.RCode != dnsmessage.RCodeSuccess {
		t.Errorf("the udp question behind %d idle connections got %+v, want an answer", maxInflight, got.header)
	}
}

func TestNewRefusesAConfigWithAPieceMissing(t *testing.T) {
	director := &fakeDirector{}
	logger := log.New(io.Discard, "", 0)
	upstream := netip.MustParseAddrPort("127.0.0.1:53")

	for name, cfg := range map[string]Config{
		"no director": {Address: netip.MustParseAddr("127.0.0.1"), Upstreams: []netip.AddrPort{upstream}, Log: logger},
		"no log":      {Address: netip.MustParseAddr("127.0.0.1"), Upstreams: []netip.AddrPort{upstream}, Director: director},
		"no address":  {Upstreams: []netip.AddrPort{upstream}, Director: director, Log: logger},
		"no upstream": {Address: netip.MustParseAddr("127.0.0.1"), Director: director, Log: logger},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New accepted the config", name)
		}
	}
}
