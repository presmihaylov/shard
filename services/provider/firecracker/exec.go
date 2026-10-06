package firecracker

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/pty"
	"github.com/presmihaylov/shard/services/runspec"
	"github.com/presmihaylov/shard/services/supervisor"
)

// Exec runs one command in a sandbox that already runs, over its own vsock connection to shard-init.
func (p *Provider) Exec(ctx context.Context, id string, spec models.ExecSpec) (models.ExitStatus, error) {
	if len(spec.Argv) == 0 {
		return models.ExitStatus{}, fmt.Errorf("sandbox %s: exec has no command to run", id)
	}

	m, r, err := p.running(ctx, id)
	if err != nil {
		return models.ExitStatus{}, err
	}
	// The guest gives the same answer, but the dial waits on a vmm busy with the snapshot or a stream still to be dialed again.
	if verb := m.holder.Load(); verb != nil {
		return models.ExitStatus{}, &models.CommandNotStartedError{Sandbox: id, Reason: fmt.Sprintf("a %s holds the sandbox frozen, and nothing starts in it until that ends: run the command again", *verb), Code: models.CommandNotExecutableExitCode}
	}

	header, err := headerOf(r, spec)
	if err != nil {
		return models.ExitStatus{}, fmt.Errorf("sandbox %s: %w", id, err)
	}

	var conn net.Conn
	dial := func(ctx context.Context, port uint32) (net.Conn, error) {
		opened, err := m.dial(ctx, port)
		if err != nil {
			return nil, err
		}
		conn = opened
		m.openExec(opened)

		return opened, nil
	}
	exit, err := supervisor.Exec(ctx, dial, id, header, spec)
	if conn == nil {
		return exit, err
	}
	if verb := m.closeExec(conn); err != nil && verb != "" {
		return exit, fmt.Errorf("sandbox %s: the %s reset the stream of this exec, so its output and exit status are lost while the command runs on in the sandbox: %w", id, verb, err)
	}

	return exit, err
}

// openExec tracks an exec stream, so a snapshot that resets it can end it.
func (m *machine) openExec(conn net.Conn) {
	m.execsMu.Lock()
	defer m.execsMu.Unlock()
	if m.execs == nil {
		m.execs = map[net.Conn]string{}
	}
	m.execs[conn] = ""
}

// closeExec stops tracking an exec stream and says which verb cut it, "" for none.
func (m *machine) closeExec(conn net.Conn) string {
	m.execsMu.Lock()
	defer m.execsMu.Unlock()
	verb := m.execs[conn]
	delete(m.execs, conn)

	return verb
}

// cutExecs ends every exec stream open across verb's snapshot create, which reset them: the host only reads, so it would wait on each for good.
func (m *machine) cutExecs(verb string) error {
	m.execsMu.Lock()
	defer m.execsMu.Unlock()
	var errs []error
	for conn, cut := range m.execs {
		if cut != "" {
			continue
		}
		m.execs[conn] = verb
		// An exec that ended closed its own stream and is about to let it go.
		if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, fmt.Errorf("sandbox %s: end an exec stream the %s reset: %w", m.id, verb, err))
		}
	}

	return errors.Join(errs...)
}

// running finds the sandbox's live guest, and refuses anything else by name and state, as an exec needs.
func (p *Provider) running(ctx context.Context, id string) (*machine, record, error) {
	dir, err := p.dir(id)
	if err != nil {
		return nil, record{}, err
	}
	r, found, err := readRecord(dir)
	if err != nil {
		return nil, record{}, err
	}
	if !found {
		return nil, record{}, fmt.Errorf("sandbox %s does not exist on %s", id, Name)
	}
	m, err := p.lookup(ctx, id, dir, r)
	if err != nil {
		return nil, record{}, err
	}
	status := models.Status{State: models.StateStopped}
	if m != nil {
		status = m.status(p)
	}
	if status.State != models.StateRunning {
		return nil, record{}, fmt.Errorf("sandbox %s is %s on %s, so nothing can run in it%s", id, status.State, Name, because(status))
	}

	return m, r, nil
}

// headerOf puts the exec where the entrypoint runs: its env under the overrides, its workdir and its user unless the spec names one.
func headerOf(r record, spec models.ExecSpec) (supervisor.ExecHeader, error) {
	header := supervisor.ExecHeader{
		Argv:    spec.Argv,
		Env:     runspec.ExecEnv(r.Run.Env, spec.Env, spec.TTY),
		WorkDir: firstNonEmpty(spec.WorkDir, r.Run.WorkDir, "/"),
		User:    r.Run.User,
		Groups:  r.Run.Groups,
		TTY:     spec.TTY,
	}
	// The guest resolves a named user, because the image on the host misses a user the sandbox added (SHARD-356).
	if spec.User != "" {
		header.User, header.Groups, header.Lookup = spec.User, nil, true
	}
	if spec.TTY && spec.Stdin != nil {
		size, err := pty.SizeOf(spec.Stdin)
		if err != nil {
			return supervisor.ExecHeader{}, fmt.Errorf("read the terminal size: %w", err)
		}
		header.Rows, header.Cols = size.Rows, size.Cols
	}

	return header, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}

	return ""
}

// Signal sends one signal to a running exec by the guest pid the exec reported for it.
func (p *Provider) Signal(ctx context.Context, id string, pid int, signal string) error {
	m, _, err := p.running(ctx, id)
	if err != nil {
		return err
	}
	if err := refuseHeld(m, "the signal"); err != nil {
		return err
	}
	if err := m.control.Load().Signal(ctx, pid, signal); err != nil {
		return fmt.Errorf("sandbox %s: %w", id, err)
	}

	return nil
}

// StopApp asks the guest supervisor to cancel every start again and term the app, or kill it with force.
func (p *Provider) StopApp(ctx context.Context, id string, force bool) error {
	m, _, err := p.running(ctx, id)
	if err != nil {
		return err
	}
	if err := refuseHeld(m, "the app stop"); err != nil {
		return err
	}
	if err := m.control.Load().StopApp(ctx, force); err != nil {
		return fmt.Errorf("sandbox %s: stop the app: %w", id, err)
	}

	return nil
}

// refuseHeld names the verb that holds the guest, whose snapshot resets the control stream, so the caller knows what went unsent rather than meet a stream error (SHARD-562).
func refuseHeld(m *machine, what string) error {
	verb := m.holder.Load()
	if verb == nil {
		return nil
	}

	return fmt.Errorf("sandbox %s: a %s holds the sandbox frozen, so %s was not sent: send it again once that ends", m.id, *verb, what)
}
