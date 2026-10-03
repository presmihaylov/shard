// Package lograte bounds the lines each source can put in a log, so a guest that floods a listener with faults cannot fill the host disk.
package lograte

import (
	"log"
	"net/netip"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	// perSecond and burst are the bound of the netstack's drop reports, per source.
	perSecond = 2
	burst     = 10
	// maxSources bounds the sources tracked at once; past it, the rest share the bound of the invalid address.
	maxSources = 1024
	// tallyAfter is how long held lines wait for the line that counts them.
	tallyAfter = time.Second
)

// Log writes a source's lines up to its bound, and a later line counts the ones it held back, so none drops unseen.
type Log struct {
	out    *log.Logger
	prefix string
	tally  time.Duration

	mu      sync.Mutex
	sources map[netip.Addr]*source
	// swept is when track last forgot idle sources, so a flood of new ones sweeps once a tally, not once a line.
	swept time.Time
}

type source struct {
	limiter *rate.Limiter
	held    int
}

// New writes through out, and starts each count line with prefix, the way the caller starts its own.
func New(out *log.Logger, prefix string) *Log {
	return &Log{out: out, prefix: prefix, tally: tallyAfter, sources: map[netip.Addr]*source{}}
}

// Printf writes one line that from caused, or holds it once from is past its bound.
func (l *Log) Printf(from netip.Addr, format string, args ...any) {
	if !l.allow(from) {
		return
	}
	l.out.Printf(format, args...)
}

// Logger is for a library that logs on its own: each line is charged to the source that sourceOf reads in it.
func (l *Log) Logger(sourceOf func(line string) netip.Addr) *log.Logger {
	return log.New(writer{log: l, sourceOf: sourceOf}, "", 0)
}

type writer struct {
	log      *Log
	sourceOf func(line string) netip.Addr
}

func (w writer) Write(p []byte) (int, error) {
	w.log.Printf(w.sourceOf(string(p)), "%s", p)

	return len(p), nil
}

func (l *Log) allow(from netip.Addr) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	from = l.track(from)
	s := l.sources[from]
	if s.limiter.Allow() {
		return true
	}
	s.held++
	if s.held == 1 {
		time.AfterFunc(l.tally, func() { l.count(from) })
	}

	return false
}

// track answers the key from is charged to, and forgets idle sources to make room for a new one.
func (l *Log) track(from netip.Addr) netip.Addr {
	if _, ok := l.sources[from]; ok {
		return from
	}
	if len(l.sources) >= maxSources && time.Since(l.swept) >= l.tally {
		l.swept = time.Now()
		for addr, s := range l.sources {
			// A full bucket with nothing held is the same as a new one.
			if s.held == 0 && s.limiter.Tokens() >= burst {
				delete(l.sources, addr)
			}
		}
	}
	if len(l.sources) >= maxSources {
		from = netip.Addr{}
		if _, ok := l.sources[from]; ok {
			return from
		}
	}
	l.sources[from] = &source{limiter: rate.NewLimiter(perSecond, burst)}

	return from
}

// count names what a source's bound held back since the last count.
func (l *Log) count(from netip.Addr) {
	l.mu.Lock()
	held := l.sources[from].held
	l.sources[from].held = 0
	l.mu.Unlock()

	name := "other sources"
	if from.IsValid() {
		name = from.String()
	}
	l.out.Printf("%s: %s: held back %d lines past the log bound", l.prefix, name, held)
}
