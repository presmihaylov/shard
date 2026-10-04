package runc

import (
	"errors"

	"github.com/presmihaylov/shard/pkg/launch"
)

type execHandle struct {
	id      string
	channel *launch.Channel
}

// A handle never repeats while a session can still hold it after its command ended.
func (r *Runner) trackExec(id string, channel *launch.Channel) (int, error) {
	r.execMu.Lock()
	defer r.execMu.Unlock()
	next := r.nextExec + 1
	if next <= 0 {
		return 0, errors.New("the exec handle limit was reached")
	}
	r.nextExec = next
	if r.execs == nil {
		r.execs = make(map[int]execHandle)
	}
	r.execs[next] = execHandle{id: id, channel: channel}

	return next, nil
}

func (r *Runner) forgetExec(handle int) {
	r.execMu.Lock()
	defer r.execMu.Unlock()
	delete(r.execs, handle)
}
