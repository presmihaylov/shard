package sandboxstate

import (
	"crypto/rand"
	"fmt"
)

// Each list holds 64 words, so a byte maps onto one without bias.
const wordsPerList = 64

var adjectives = [wordsPerList]string{
	"amber", "ancient", "autumn", "bitter", "blue", "bold", "brave", "bright",
	"broken", "calm", "cold", "cool", "crimson", "curly", "damp", "dark",
	"dawn", "delicate", "divine", "dry", "empty", "falling", "fancy", "flat",
	"floral", "fragrant", "frosty", "gentle", "green", "hidden", "holy", "icy",
	"jolly", "late", "lively", "long", "lucky", "misty", "morning", "muddy",
	"nameless", "noisy", "odd", "old", "orange", "patient", "plain", "polished",
	"proud", "purple", "quiet", "rapid", "red", "restless", "rough", "royal",
	"shy", "silent", "small", "snowy", "solitary", "sparkling", "spring", "steep",
}

var nouns = [wordsPerList]string{
	"anchor", "badger", "beaver", "bird", "brook", "butterfly", "cherry", "cloud",
	"comet", "crater", "creek", "dawn", "dew", "dream", "dust", "ember",
	"falcon", "feather", "fern", "field", "fire", "firefly", "flower", "fog",
	"forest", "fox", "frog", "frost", "glade", "glitter", "grass", "hare",
	"haze", "heron", "hill", "lake", "leaf", "lion", "meadow", "moon",
	"morning", "mountain", "night", "otter", "owl", "paper", "pebble", "pine",
	"pond", "rain", "resonance", "river", "salad", "shadow", "shape", "silence",
	"sky", "smoke", "snow", "sound", "star", "sun", "surf", "thunder",
}

// generateID reads as words because an operator types it. The suffix keeps a repeat rare, and the
// mkdir that claims the id is what makes a repeat impossible.
func generateID() (string, error) {
	var b [4]byte

	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}

	return fmt.Sprintf("%s-%s-%02x%02x",
		adjectives[int(b[0])%wordsPerList], nouns[int(b[1])%wordsPerList], b[2], b[3]), nil
}
