package sandboxstate

import (
	"regexp"
	"testing"
)

var idShape = regexp.MustCompile(`^[a-z]+-[a-z]+-[0-9a-f]{4}$`)

// stubIDs makes newID hand out ids in order, so a test can force the collision only mkdir refuses.
func stubIDs(t *testing.T, ids ...string) {
	t.Helper()

	next := 0
	newID = func() (string, error) {
		id := ids[min(next, len(ids)-1)]
		next++

		return id, nil
	}

	t.Cleanup(func() { newID = generateID })
}

func TestAClaimedIDIsNeverHandedOutTwice(t *testing.T) {
	r, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	stubIDs(t, "quiet-heron-0001", "quiet-heron-0001", "bold-otter-0002")

	first, err := r.claimID()
	if err != nil {
		t.Fatalf("the first claim: %v", err)
	}

	second, err := r.claimID()
	if err != nil {
		t.Fatalf("the second claim: %v", err)
	}

	if first != "quiet-heron-0001" || second != "bold-otter-0002" {
		t.Errorf("the claims are %q and %q, want the second one to skip the taken id", first, second)
	}
}

func TestAClaimGivesUpWhenEveryIDIsTaken(t *testing.T) {
	r, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	stubIDs(t, "quiet-heron-0001")

	if _, err := r.claimID(); err != nil {
		t.Fatalf("the first claim: %v", err)
	}

	if _, err := r.claimID(); err == nil {
		t.Fatal("the second claim went through, and every attempt drew an id that was already taken")
	}
}

func TestGeneratedIDsReadAsWords(t *testing.T) {
	for range 100 {
		id, err := generateID()
		if err != nil {
			t.Fatalf("generateID: %v", err)
		}

		if !idShape.MatchString(id) {
			t.Fatalf("the id %q does not read as adjective-noun-suffix", id)
		}

		if err := validID(id); err != nil {
			t.Fatalf("the generated id %q is not a valid one: %v", id, err)
		}
	}
}

// The space is 64 x 64 x 65536, so 100 draws repeat about once in fifty thousand runs.
func TestGeneratedIDsRarelyRepeat(t *testing.T) {
	seen := map[string]bool{}

	for range 100 {
		id, err := generateID()
		if err != nil {
			t.Fatalf("generateID: %v", err)
		}

		if seen[id] {
			t.Fatalf("the id %q came up twice in a hundred draws", id)
		}

		seen[id] = true
	}
}

func TestTheWordListsHoldNoDuplicates(t *testing.T) {
	for _, list := range [][wordsPerList]string{adjectives, nouns} {
		seen := map[string]bool{}

		for _, word := range list {
			if seen[word] {
				t.Errorf("the word %q is in the list twice, which skews the draw", word)
			}

			seen[word] = true
		}
	}
}
