package daemon

import (
	"strings"
	"testing"
)

func TestTheDaemonHoldsAStorageSizeToTheMinimum(t *testing.T) {
	t.Parallel()

	if got, err := checkStorage(nil); got != 0 || err != nil {
		t.Errorf("no size gave %d, %v; want 0 and no error", got, err)
	}
	ten := int64(10 << 10)
	if got, err := checkStorage(&ten); got != ten || err != nil {
		t.Errorf("10 GiB gave %d, %v", got, err)
	}
	for _, mib := range []int64{0, 9 << 10} {
		_, err := checkStorage(&mib)
		if err == nil || !strings.Contains(err.Error(), "below the minimum of 10 GiB") {
			t.Errorf("%d MiB gave %v", mib, err)
		}
	}
}
