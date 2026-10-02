package image

import (
	"context"
	"slices"
	"sync"
)

// The statuses of a pull event, in the order a pull says them; cached is the whole story of an image already on disk.
const (
	StatusCached  = "cached"
	StatusPulling = "pulling"
	StatusLayer   = "layer"
	StatusPulled  = "pulled"
)

// Event is one step of a pull, as the daemon streams it to a client.
type Event struct {
	Status    string `json:"status"`
	Reference string `json:"reference,omitempty"`
	// Digest names the image, except on a layer event, where it names the layer.
	Digest string `json:"digest,omitempty"`
	Layers int    `json:"layers,omitempty"`
	// Bytes is the download: the whole image on pulling, one blob on layer.
	Bytes int64 `json:"bytes,omitempty"`
	// Present is a layer another image already brought, so it cost no download.
	Present bool `json:"present,omitempty"`
	// Path is where the image went, on pulled and cached.
	Path string `json:"path,omitempty"`
}

// Progress is what the pulls under one request said; a pull never waits on a reader, so a lagging client costs it nothing.
type Progress struct {
	mu      sync.Mutex
	events  []Event
	closed  bool
	changed chan struct{}
}

func NewProgress() *Progress {
	return &Progress{changed: make(chan struct{})}
}

type progressKey struct{}

// WithProgress has every pull under ctx report to p, the way httptrace hangs a trace on a request.
func WithProgress(ctx context.Context, p *Progress) context.Context {
	if p == nil {
		return ctx
	}

	return context.WithValue(ctx, progressKey{}, p)
}

// ProgressFrom is the log ctx carries, or nil, which every method takes as quiet.
func ProgressFrom(ctx context.Context) *Progress {
	p, _ := ctx.Value(progressKey{}).(*Progress)

	return p
}

// Add puts e on the log and wakes every follower; it never waits on one, and a nil log drops it.
func (p *Progress) Add(e Event) {
	if p == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return
	}
	p.events = append(p.events, e)
	p.wake()
}

// Close ends the log once the work that writes it has returned, so a follower drains it and stops.
func (p *Progress) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return
	}
	p.closed = true
	p.wake()
}

func (p *Progress) wake() {
	close(p.changed)
	p.changed = make(chan struct{})
}

// Follow yields every event from the first, then each one as it lands, until the log closes or ctx ends.
func (p *Progress) Follow(ctx context.Context, yield func(Event) error) error {
	next := 0
	for {
		p.mu.Lock()
		events := slices.Clone(p.events[next:])
		closed, changed := p.closed, p.changed
		p.mu.Unlock()

		for _, e := range events {
			if err := yield(e); err != nil {
				return err
			}
		}
		next += len(events)

		if closed {
			return nil
		}

		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// pullReport turns what the registry store hears into events on p.
type pullReport struct {
	p *Progress
}

func (r pullReport) Manifest(ref, digest string, sizes []int64) {
	var total int64
	for _, size := range sizes {
		total += size
	}

	r.p.Add(Event{Status: StatusPulling, Reference: ref, Digest: digest, Layers: len(sizes), Bytes: total})
}

func (r pullReport) Layer(digest string, size int64, present bool) {
	r.p.Add(Event{Status: StatusLayer, Digest: digest, Bytes: size, Present: present})
}

// landed says where img went: its rootfs, or the image file a VM provider boots from.
func landed(status string, img Image) Event {
	path := img.RootFS
	if img.Disk != "" {
		path = img.Disk
	}
	if img.Erofs != "" {
		path = img.Erofs
	}

	return Event{Status: status, Reference: img.Reference, Digest: img.Digest, Path: path}
}
