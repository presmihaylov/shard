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

	// addresses is the sandbox behind each address, rebuilt on a miss: a sandbox that has just started
	// is the ordinary miss, and a drop names an address and never an id.
	addresses map[string]models.Sandbox
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

		sb, found := t.sandboxAt(record.source)
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
			return err
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

// sandboxAt answers which sandbox holds an address.
func (t *Tailer) sandboxAt(address string) (models.Sandbox, bool) {
	sb, ok := t.addresses[address]
	if ok {
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
	t.addresses = map[string]models.Sandbox{}
	for _, each := range sandboxes {
		if each.Address.IsValid() {
			t.addresses[each.Address.Addr().String()] = each
		}
	}

	sb, ok = t.addresses[address]

	return sb, ok
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
