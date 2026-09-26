package store

import "fmt"

// Exchange swaps what is at a with what is at b in one step, so a reader of either path never finds neither; a filesystem without it says so.
func Exchange(a, b string) error {
	if err := exchange(a, b); err != nil {
		return fmt.Errorf("exchange %s and %s: %w", a, b, err)
	}

	return nil
}
