// Package linkfwd carries a sandbox's flow to one of its link addresses into the target: port 22 to the daemon's ssh server, any other port to the target's loopback.
package linkfwd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/netns"
	"github.com/presmihaylov/shard/services/network"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/sandboxstate"
	"github.com/presmihaylov/shard/services/sshd"
)

// SSHPort is the link port the ssh server answers instead of the target's own loopback.
const SSHPort = 22

// dialBudget bounds the reach into the target, as a host port forward's does.
const dialBudget = 15 * time.Second

// ErrNoLink is a flow whose source holds no link to the address it dialed.
var ErrNoLink = errors.New("no link")

// Records reads the sandboxes, whose links say which flows the forwarder carries.
type Records interface {
	Get(id string) (models.Sandbox, error)
	List() ([]models.Sandbox, error)
}

// Sandboxes reaches a target through the sandbox service, whose readiness check refuses a stopped one.
type Sandboxes interface {
	ReadyForSSH(ctx context.Context, ref string) (string, error)
	DialPort(ctx context.Context, ref string, port uint16) (net.Conn, error)
}

// SSH serves one ssh connection into a sandbox.
type SSH interface {
	Serve(ctx context.Context, conn net.Conn, opts sshd.Options) error
}

// Forwarder carries link flows and ends the ones a link change took away.
type Forwarder struct {
	records   Records
	sandboxes Sandboxes
	ssh       SSH
	report    func(string)
	// original reads where a dnat'd connection was going; tests stand in for conntrack.
	original func(net.Conn) (netip.AddrPort, error)

	// mu guards flows. Changed holds it across its record read, so a dial either registered first or reads the new links.
	mu    sync.Mutex
	flows map[*flow]struct{}
	// sessions are the port 22 flows' ssh servers, which Serve waits out.
	sessions sync.WaitGroup
}

// flow is one carried connection; conn is nil while its dial is in flight, and closed tells that dial to give up.
type flow struct {
	source string
	target string
	conn   net.Conn
	closed bool
}

// New builds a forwarder with no flow; report takes what broke after the guest could be told.
func New(records Records, sandboxes Sandboxes, ssh SSH, report func(string)) *Forwarder {
	return &Forwarder{records: records, sandboxes: sandboxes, ssh: ssh, report: report, original: netns.OriginalDestination, flows: map[*flow]struct{}{}}
}

// Dial opens the target end of a flow from the sandbox at source to dst, which must be one of that sandbox's link addresses.
func (f *Forwarder) Dial(ctx context.Context, source netip.Addr, dst netip.AddrPort) (net.Conn, error) {
	id, err := f.sourceOf(source)
	if err != nil {
		return nil, err
	}

	fl, err := f.track(id, dst.Addr())
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, dialBudget)
	defer cancel()
	conn, err := f.open(ctx, fl.target, dst.Port())
	if err != nil {
		f.untrack(fl)

		return nil, fmt.Errorf("sandbox %s link to %s: %w", id, dst, err)
	}

	return f.hold(fl, conn)
}

// sourceOf is the sandbox that holds addr; a failed one may have given its address to another, so it holds none.
func (f *Forwarder) sourceOf(addr netip.Addr) (string, error) {
	sandboxes, err := sandboxstate.ListReadable(f.records, nil)
	if err != nil {
		return "", err
	}

	for _, sb := range sandboxes {
		if sb.State != models.StateFailed && sb.Address.IsValid() && sb.Address.Addr() == addr {
			return sb.ID, nil
		}
	}

	return "", fmt.Errorf("no sandbox holds %s: %w", addr, ErrNoLink)
}

// track registers a flow to the source's link at addr, read under mu so a Changed that follows sees it.
func (f *Forwarder) track(source string, addr netip.Addr) (*flow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	sb, err := f.records.Get(source)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(sb.Links, func(l models.Link) bool { return l.Address == addr })
	if i < 0 {
		return nil, fmt.Errorf("sandbox %s holds no link to %s: %w", source, addr, ErrNoLink)
	}

	fl := &flow{source: source, target: sb.Links[i].TargetID}
	f.flows[fl] = struct{}{}

	return fl, nil
}

func (f *Forwarder) open(ctx context.Context, target string, port uint16) (net.Conn, error) {
	if port != SSHPort {
		return f.sandboxes.DialPort(ctx, target, port)
	}

	// Refused before the handshake, so a stopped target answers a link's ssh the way it answers any other port.
	id, err := f.sandboxes.ReadyForSSH(ctx, target)
	if err != nil {
		return nil, err
	}

	client, server := net.Pipe()
	// The session outlives the dial: it ends when the flow closes its end, or the ssh server gives up.
	serveCtx := context.WithoutCancel(ctx)
	f.sessions.Go(func() {
		if err := f.ssh.Serve(serveCtx, server, sshd.Options{Sandbox: id, Explain: f.explain}); err != nil {
			f.report(fmt.Sprintf("link: ssh into sandbox %s: %v", id, err))
		}
	})

	return client, nil
}

// explain shows a link's ssh client what a refusal made public, since the guest is no operator to read the host's detail.
func (f *Forwarder) explain(err error) string {
	if public, ok := sandbox.PublicText(err); ok {
		return public
	}
	f.report(fmt.Sprintf("link: ssh refused: %v", err))

	return "shard could not do that; the daemon log has why"
}

// hold keeps conn as the flow's, unless a Changed ended the flow while the dial was in flight.
func (f *Forwarder) hold(fl *flow, conn net.Conn) (net.Conn, error) {
	f.mu.Lock()
	ended := fl.closed
	if !ended {
		fl.conn = conn
	}
	f.mu.Unlock()

	if ended {
		return nil, errors.Join(fmt.Errorf("sandbox %s lost its link to %s: %w", fl.source, fl.target, ErrNoLink), quiet(conn.Close()))
	}

	return wrap(f, fl, conn), nil
}

func (f *Forwarder) untrack(fl *flow) {
	f.mu.Lock()
	defer f.mu.Unlock()

	delete(f.flows, fl)
}

// Changed closes every flow from the sandbox to a target it no longer links, and all of them once its record is gone.
func (f *Forwarder) Changed(_ context.Context, id string) error {
	f.mu.Lock()
	linked := map[string]bool{}
	sb, err := f.records.Get(id)
	// A record that cannot be read links nothing it can prove, so its flows end; only a missing one is no error.
	if errors.Is(err, sandboxstate.ErrNotFound) {
		err = nil
	}
	for _, l := range sb.Links {
		linked[l.TargetID] = true
	}

	var ended []net.Conn
	for fl := range f.flows {
		if fl.source != id || linked[fl.target] {
			continue
		}
		fl.closed = true
		delete(f.flows, fl)
		if fl.conn != nil {
			ended = append(ended, fl.conn)
		}
	}
	f.mu.Unlock()

	// Closed outside mu: the flow's own close takes it.
	errs := []error{err}
	for _, conn := range ended {
		errs = append(errs, quiet(conn.Close()))
	}

	return errors.Join(errs...)
}

func quiet(err error) error {
	if errors.Is(err, net.ErrClosed) {
		return nil
	}

	return err
}

// Routes is the link half of the host rules: each sandbox with an address and the link addresses it reaches.
type Routes struct {
	Records sandboxstate.Lister
}

func (r Routes) Linked(context.Context) ([]network.Linked, error) {
	sandboxes, err := sandboxstate.ListReadable(r.Records, nil)
	if err != nil {
		return nil, err
	}

	var linked []network.Linked
	for _, sb := range sandboxes {
		// A lease outlives a stop, so a stopped source keeps its rules, and a failed one may have given its address away.
		if len(sb.Links) == 0 || !sb.Address.IsValid() || sb.State == models.StateFailed {
			continue
		}
		targets := make([]netip.Addr, 0, len(sb.Links))
		for _, l := range sb.Links {
			targets = append(targets, l.Address)
		}
		linked = append(linked, network.Linked{Source: sb.Address.Addr(), Targets: targets})
	}
	slices.SortFunc(linked, func(a, b network.Linked) int { return a.Source.Compare(b.Source) })

	return linked, nil
}
