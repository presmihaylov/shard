package daemon

import (
	"sync"
	"testing"
)

// The daemon supervises its tasks at once over one deps, so two of them can ask for the same layer at
// the same moment. Run this one under -race: without the lock the detector names the memoised fields.
func TestTheGettersBuildOneLayerUnderConcurrentAsks(t *testing.T) {
	d := &deps{cfg: Config{Root: t.TempDir()}}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			repo, err := d.repo()
			if err != nil {
				t.Errorf("repo: %v", err)
			}

			secrets, err := d.secrets()
			if err != nil {
				t.Errorf("secrets: %v", err)
			}

			if _, err := d.egress(); err != nil {
				t.Errorf("egress: %v", err)
			}

			// One daemon holds one of each, or the proxy judges against a store the API never writes to.
			if repo != d.repoSvc || secrets != d.secretSvc {
				t.Error("a getter built a second copy of a layer the daemon already held")
			}
		})
	}
	wg.Wait()
}
