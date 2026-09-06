//go:build !linux

package kmsg

import "context"

// Follower is the ring off Linux, where there is none: every verb refuses.
type Follower struct{}

func Open() (*Follower, error) { return nil, ErrNotSupported }

func (f *Follower) Close() error { return ErrNotSupported }

func (f *Follower) Overwritten() uint64 { return 0 }

func (f *Follower) Follow(context.Context, func(Record) error, func()) error { return ErrNotSupported }
