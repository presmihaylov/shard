package state

import (
	"regexp"
	"testing"
)

var idShape = regexp.MustCompile(`^[a-z]+-[a-z]+-[0-9a-f]{4}$`)

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
