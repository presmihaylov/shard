package sandboxstate_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

func named(t *testing.T, r *sandboxstate.Repository, name string) models.Sandbox {
	t.Helper()

	sb := newSandbox()
	sb.Name = name

	out, err := r.Create(sb)
	if err != nil {
		t.Fatalf("Create %q: %v", name, err)
	}

	return out
}

func TestResolveAnswersForANameAndForAnID(t *testing.T) {
	r, _ := repo(t)

	sb := named(t, r, "builder")

	for _, ref := range []string{"builder", sb.ID} {
		id, err := r.Resolve(ref)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", ref, err)
		}
		if id != sb.ID {
			t.Fatalf("Resolve(%q) = %q, want %q", ref, id, sb.ID)
		}
	}
}

func TestResolveLeavesAReferenceNothingHolds(t *testing.T) {
	r, _ := repo(t)

	id, err := r.Resolve("nothing-holds-this")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if id != "nothing-holds-this" {
		t.Fatalf("Resolve = %q, want it unchanged", id)
	}
}

func TestCreateRefusesANameThatIsTaken(t *testing.T) {
	r, _ := repo(t)

	first := named(t, r, "builder")

	sb := newSandbox()
	sb.Name = "builder"

	_, err := r.Create(sb)
	if err == nil {
		t.Fatal("Create took the same name twice")
	}
	if !strings.Contains(err.Error(), first.ID) {
		t.Fatalf("the collision error does not name the holder %s: %v", first.ID, err)
	}
}

func TestCreateGivesTheIDBackWhenTheNameIsTaken(t *testing.T) {
	r, root := repo(t)

	named(t, r, "builder")

	sb := newSandbox()
	sb.Name = "builder"
	if _, err := r.Create(sb); err == nil {
		t.Fatal("Create took the same name twice")
	}

	left, err := os.ReadDir(filepath.Join(root, "sandboxes"))
	if err != nil {
		t.Fatalf("read the sandboxes directory: %v", err)
	}
	if len(left) != 1 {
		t.Fatalf("the refused create left %d sandbox directories, want 1", len(left))
	}
}

func TestCreateRefusesANameSpelledLikeAGeneratedID(t *testing.T) {
	r, _ := repo(t)

	sb := newSandbox()
	sb.Name = "dawn-flower-0c28"

	if _, err := r.Create(sb); err == nil {
		t.Fatal("Create took a name spelled like an id")
	}
}

func TestCreateRefusesANameWithASeparatorInIt(t *testing.T) {
	r, _ := repo(t)

	for _, name := range []string{"../escape", "a/b", ""} {
		sb := newSandbox()
		sb.Name = name

		// An empty name is no name at all, so it is the one that must be taken.
		_, err := r.Create(sb)
		if name == "" && err != nil {
			t.Fatalf("Create refused the empty name: %v", err)
		}
		if name != "" && err == nil {
			t.Fatalf("Create took the name %q", name)
		}
	}
}

func TestDeleteGivesTheNameBack(t *testing.T) {
	r, _ := repo(t)

	sb := named(t, r, "builder")
	if err := r.Delete(sb.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	again := named(t, r, "builder")
	if again.ID == sb.ID {
		t.Fatal("the second create reused the first id, so this proves nothing")
	}

	id, err := r.Resolve("builder")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if id != again.ID {
		t.Fatalf("Resolve = %q, want the second sandbox %q", id, again.ID)
	}
}

func TestUpdateRefusesARename(t *testing.T) {
	r, _ := repo(t)

	sb := named(t, r, "builder")

	err := r.Update(sb.ID, func(sb *models.Sandbox) error {
		sb.Name = "other"

		return nil
	})
	if err == nil {
		t.Fatal("Update renamed a sandbox")
	}
}

func TestAnUnnamedSandboxClaimsNoName(t *testing.T) {
	r, root := repo(t)

	create(t, r)

	links, err := os.ReadDir(filepath.Join(root, "names"))
	if err != nil {
		t.Fatalf("read the names directory: %v", err)
	}
	if len(links) != 0 {
		t.Fatalf("an unnamed sandbox claimed %d names", len(links))
	}
}

func TestOneCreateWinsARacedName(t *testing.T) {
	r, root := repo(t)

	const racers = 8

	var start sync.WaitGroup
	start.Add(1)

	var done sync.WaitGroup
	won := make(chan string, racers)

	for range racers {
		done.Go(func() {
			start.Wait()

			sb := newSandbox()
			sb.Name = "builder"

			out, err := r.Create(sb)
			if err == nil {
				won <- out.ID
			}
		})
	}

	start.Done()
	done.Wait()
	close(won)

	if len(won) != 1 {
		t.Fatalf("%d of %d creates took the same name, want 1", len(won), racers)
	}

	left, err := os.ReadDir(filepath.Join(root, "sandboxes"))
	if err != nil {
		t.Fatalf("read the sandboxes directory: %v", err)
	}
	if len(left) != 1 {
		t.Fatalf("the race left %d sandbox directories, want 1", len(left))
	}
}

// TestDeleteLeavesTheNameOfANewerSandbox is the half-done delete: the link is gone but the record
// is not, so a second create takes the name, and the retried delete must not unlink it.
func TestDeleteLeavesTheNameOfANewerSandbox(t *testing.T) {
	r, root := repo(t)

	first := named(t, r, "builder")

	if err := os.Remove(filepath.Join(root, "names", "builder")); err != nil {
		t.Fatalf("simulate the half-done delete: %v", err)
	}

	second := named(t, r, "builder")

	if err := r.Delete(first.ID); err != nil {
		t.Fatalf("Delete the first sandbox: %v", err)
	}

	id, err := r.Resolve("builder")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if id != second.ID {
		t.Fatalf("Resolve = %q, want the second sandbox %q: the delete stole its name", id, second.ID)
	}
}

// TestACrashBeforeTheNameClaimCostsOnlyTheName is the other half: the record is on disk and the link
// is not, so the name is free and the sandbox is still reachable by its id.
func TestACrashBeforeTheNameClaimCostsOnlyTheName(t *testing.T) {
	r, root := repo(t)

	sb := named(t, r, "builder")

	if err := os.Remove(filepath.Join(root, "names", "builder")); err != nil {
		t.Fatalf("simulate the crash: %v", err)
	}

	if _, err := r.Get(sb.ID); err != nil {
		t.Fatalf("Get by id: %v", err)
	}

	other := named(t, r, "builder")
	if other.ID == sb.ID {
		t.Fatal("the second create reused the first id, so this proves nothing")
	}

	if err := r.Delete(sb.ID); err != nil {
		t.Fatalf("Delete the sandbox that lost its name: %v", err)
	}
}

func TestResolveReportsAnUnreadableNamesDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory whatever its mode says")
	}

	r, root := repo(t)

	named(t, r, "builder")

	names := filepath.Join(root, "names")
	if err := os.Chmod(names, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(names, 0o700); err != nil {
			t.Fatalf("restore the mode: %v", err)
		}
	})

	if _, err := r.Resolve("builder"); err == nil {
		t.Fatal("Resolve read an unreadable directory as no such name")
	}
}
