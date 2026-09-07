package egress

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/kmsg"
	"github.com/presmihaylov/shard/pkg/store"
)

// CursorFile holds the last kernel sequence the tailer wrote, so a daemon that restarts writes the
// ring's backlog once and no more.
const CursorFile = "egress.cursor"

const cursorPerm = 0o640

// Ring is the kernel ring buffer the host chains log their drops into.
type Ring interface {
	Follow(ctx context.Context, yield func(kmsg.Record) error, caughtUp func()) error
}

// Sandboxes is the part of the repository the tailer needs: which sandbox holds which address.
type Sandboxes interface {
	List() ([]models.Sandbox, error)
}

// Tailer makes a host drop as durable as a proxy decision. The ring is shared with the whole host and
// short, so a drop that is only ever read at print time is gone within minutes on a busy box.
type Tailer struct {
	root string
	log  *Log
	repo Sandboxes
	out  *log.Logger

	// holders is the sandbox behind each address and each host interface, rebuilt on a miss: a sandbox
	// that has just started is the ordinary miss, and a drop names one of those and never an id.
	holders   map[string]models.Sandbox
	refreshed time.Time
}

func NewTailer(root string, decisions *Log, repo Sandboxes, out *log.Logger) *Tailer {
	return &Tailer{root: root, log: decisions, repo: repo, out: out}
}

// refreshEvery bounds how often a miss rebuilds the address map, so a line for a sandbox that is gone
// does not list the records once per drop.
const refreshEvery = time.Second

// Run writes every host drop the ring holds into the sandbox it belongs to, then follows the ring.
func (t *Tailer) Run(ctx context.Context, ring Ring) error {
	cursor, seen := t.cursor()

	var unattributed int
	err := ring.Follow(ctx, func(line kmsg.Record) error {
		if seen && line.Sequence <= cursor {
			return nil
		}

		record, ok := hostDrop(line.Message)
		if !ok {
			return nil
		}

		sb, found := t.sandboxFor(record.source, record.iface)
		if !found {
			unattributed++

			return nil
		}
		// An address is reused, so a line older than the sandbox belongs to whoever held it before.
		if line.Time.Before(sb.CreatedAt) {
			return nil
		}

		record.Time = line.Time
		if err := t.log.Append(sb.ID, record.Record); err != nil {
			// A cached holder can name a sandbox that has just been removed, and its file went with it.
			if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			t.forget(sb)
			unattributed++

			return nil
		}

		cursor, seen = line.Sequence, true

		return t.writeCursor(line.Sequence)
	}, func() {
		if unattributed > 0 {
			t.out.Printf("egress log: %d host drops named a sandbox that no longer exists", unattributed)
		}
	})
	if errors.Is(err, context.Canceled) {
		return nil
	}

	return err
}

// sandboxFor answers whose drop this is. A routed drop names the sandbox's address, and an IPv6 one
// dies at the port before it is routed, so the port is the only thing that names it.
func (t *Tailer) sandboxFor(keys ...string) (models.Sandbox, bool) {
	if sb, ok := t.holder(keys); ok {
		return sb, true
	}
	if time.Since(t.refreshed) < refreshEvery {
		return models.Sandbox{}, false
	}

	sandboxes, err := t.repo.List()
	if err != nil {
		// A record shard cannot read names no address, so the drop is counted with the rest and not lost twice.
		t.out.Printf("egress log: the sandbox records cannot be listed: %v", err)
	}

	t.refreshed = time.Now()
	t.holders = map[string]models.Sandbox{}
	for _, each := range sandboxes {
		if each.Address.IsValid() {
			t.holders[each.Address.Addr().String()] = each
		}
		if each.HostInterface != "" {
			t.holders[each.HostInterface] = each
		}
	}

	return t.holder(keys)
}

// forget drops a sandbox the records no longer hold, so the next line rebuilds instead of hitting it.
func (t *Tailer) forget(sb models.Sandbox) {
	for key, held := range t.holders {
		if held.ID == sb.ID {
			delete(t.holders, key)
		}
	}
	t.refreshed = time.Time{}
}

// holder takes the first key that names a sandbox, so a line carrying both is read by its address.
func (t *Tailer) holder(keys []string) (models.Sandbox, bool) {
	for _, key := range keys {
		if key == "" {
			continue
		}
		if sb, ok := t.holders[key]; ok {
			return sb, true
		}
	}

	return models.Sandbox{}, false
}

// cursor reads the last sequence written. An unreadable one means write everything the ring holds: a
// line written twice is better than a drop nobody ever sees.
func (t *Tailer) cursor() (uint64, bool) {
	path := filepath.Join(t.root, CursorFile)

	blob, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false
	}
	if err != nil {
		t.out.Printf("egress log: %s cannot be read, so the ring is written whole: %v", path, err)

		return 0, false
	}

	sequence, err := strconv.ParseUint(strings.TrimSpace(string(blob)), 10, 64)
	if err != nil {
		t.out.Printf("egress log: %s holds no sequence, so the ring is written whole: %v", path, err)

		return 0, false
	}

	return sequence, true
}

func (t *Tailer) writeCursor(sequence uint64) error {
	path := filepath.Join(t.root, CursorFile)

	if err := store.WriteFile(path, []byte(strconv.FormatUint(sequence, 10)+"\n"), cursorPerm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}
