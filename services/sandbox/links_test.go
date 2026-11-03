package sandbox_test

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// heard stands in for what will serve links, and keeps every sandbox it was told changed.
type heard struct {
	mu  sync.Mutex
	ids []string
	err error
}

func (h *heard) Changed(_ context.Context, id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ids = append(h.ids, id)

	return h.err
}

func (h *heard) told() []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	return slices.Clone(h.ids)
}

// linkHost is a service over a real repository of stopped sandboxes, since a link touches more records than the fake holds.
type linkHost struct {
	svc   *sandbox.Service
	repo  *sandboxstate.Repository
	heard *heard
	ids   map[string]string
	layers
}

func newLinkHost(t *testing.T, names ...string) linkHost {
	t.Helper()

	repo, err := sandboxstate.New(t.TempDir())
	if err != nil {
		t.Fatalf("sandboxstate.New: %v", err)
	}
	h := linkHost{repo: repo, heard: &heard{}, ids: map[string]string{}}
	for _, name := range names {
		sb, err := repo.Create(models.Sandbox{Name: name, State: models.StateStopped})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		h.ids[name] = sb.ID
	}
	h.svc, h.layers = newService(t, &recorder{}, stopped(), func(cfg *sandbox.Config) {
		cfg.Repo, cfg.Links = repo, h.heard
	})

	return h
}

func (h linkHost) links(t *testing.T, name string) []models.Link {
	t.Helper()

	sb, err := h.repo.Get(h.ids[name])
	if err != nil {
		t.Fatalf("get %s: %v", name, err)
	}

	return sb.Links
}

func (h linkHost) link(t *testing.T, source, target string) models.Link {
	t.Helper()

	link, err := h.svc.AddLink(t.Context(), source, target)
	if err != nil {
		t.Fatalf("link %s to %s: %v", source, target, err)
	}

	return link
}

func (h linkHost) fail(t *testing.T, name string) {
	t.Helper()

	err := h.repo.Update(h.ids[name], func(sb *models.Sandbox) error {
		sb.State, sb.FailedReason = models.StateFailed, "the guest did not boot"

		return nil
	})
	if err != nil {
		t.Fatalf("fail %s: %v", name, err)
	}
}

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

func TestAddLinkGivesEachTargetOneAddressEverySourceShares(t *testing.T) {
	h := newLinkHost(t, "agent1", "agent2", "web1", "db1")

	for _, step := range []struct {
		source, target string
		want           netip.Addr
	}{
		{"agent1", "web1", addr("10.86.0.1")},
		{"agent1", "db1", addr("10.86.0.2")},
		{"agent2", "web1", addr("10.86.0.1")},
	} {
		link := h.link(t, step.source, step.target)
		want := models.Link{Target: step.target, TargetID: h.ids[step.target], Address: step.want}
		if link != want {
			t.Errorf("link %s to %s answered %+v, want %+v", step.source, step.target, link, want)
		}
	}

	want := []models.Link{
		{Target: "web1", TargetID: h.ids["web1"], Address: addr("10.86.0.1")},
		{Target: "db1", TargetID: h.ids["db1"], Address: addr("10.86.0.2")},
	}
	if got := h.links(t, "agent1"); !slices.Equal(got, want) {
		t.Errorf("agent1 holds %+v, want %+v", got, want)
	}
	if got, want := h.heard.told(), []string{h.ids["agent1"], h.ids["agent1"], h.ids["agent2"]}; !slices.Equal(got, want) {
		t.Errorf("the seam heard %v, want each source once per new link %v", got, want)
	}
}

func TestAddLinkOfALinkItHoldsAnswersItAndChangesNothing(t *testing.T) {
	h := newLinkHost(t, "agent1", "web1")
	first := h.link(t, "agent1", "web1")

	again := h.link(t, "agent1", h.ids["web1"])

	if again != first {
		t.Errorf("the second put answered %+v, want the link it holds %+v", again, first)
	}
	if got := h.links(t, "agent1"); len(got) != 1 {
		t.Errorf("agent1 holds %+v, want the one link", got)
	}
	if got := h.heard.told(); len(got) != 1 {
		t.Errorf("the seam heard %v, want the first put alone", got)
	}
}

func TestAddLinkRefusesWhatCannotBeLinked(t *testing.T) {
	cases := []struct {
		name           string
		source, target string
		failed         string
		check          func(error) bool
	}{
		{"itself", "agent1", "agent1", "", isRequestError},
		{"a target that does not exist", "agent1", "ghost", "", isRequestError},
		{"a target that failed", "agent1", "web1", "web1", isFailed},
		{"a source that failed", "agent1", "web1", "agent1", isFailed},
		{"a source that does not exist", "ghost", "web1", "", func(err error) bool { return errors.Is(err, sandboxstate.ErrNotFound) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newLinkHost(t, "agent1", "web1")
			if tc.failed != "" {
				h.fail(t, tc.failed)
			}

			_, err := h.svc.AddLink(t.Context(), tc.source, tc.target)

			if !tc.check(err) {
				t.Fatalf("link %s to %s = %v, want it refused", tc.source, tc.target, err)
			}
			if got := h.links(t, "agent1"); len(got) != 0 {
				t.Errorf("a refused link left %+v on the record", got)
			}
			if got := h.heard.told(); len(got) != 0 {
				t.Errorf("the seam heard %v of a refused link", got)
			}
		})
	}
}

func isFailed(err error) bool {
	refused, ok := errors.AsType[*sandbox.StateError](err)
	return ok && refused.Code == models.CodeSandboxFailed
}

func TestAddLinkTakesTheLinkBackWhenItIsNotServed(t *testing.T) {
	h := newLinkHost(t, "agent1", "web1")
	h.heard.err = errors.New("the rules did not apply")

	if _, err := h.svc.AddLink(t.Context(), "agent1", "web1"); !errors.Is(err, h.heard.err) {
		t.Fatalf("link = %v, want the seam's refusal", err)
	}
	if got := h.links(t, "agent1"); len(got) != 0 {
		t.Errorf("a link nothing serves stays on the record: %+v", got)
	}
}

func TestRemoveLinkFreesTheAddressOnceNoLinkHoldsIt(t *testing.T) {
	h := newLinkHost(t, "agent1", "agent2", "web1", "db1", "cache1", "api1")
	h.link(t, "agent1", "web1")
	h.link(t, "agent2", "web1")
	h.link(t, "agent1", "db1")

	if err := h.svc.RemoveLink(t.Context(), "agent1", "web1"); err != nil {
		t.Fatalf("remove link: %v", err)
	}
	if got := h.link(t, "agent1", "cache1").Address; got != addr("10.86.0.3") {
		t.Errorf("cache1 took %s while agent2 still links web1 at 10.86.0.1, want 10.86.0.3", got)
	}

	if err := h.svc.RemoveLink(t.Context(), "agent2", h.ids["web1"]); err != nil {
		t.Fatalf("remove link by id: %v", err)
	}
	if got := h.link(t, "agent2", "api1").Address; got != addr("10.86.0.1") {
		t.Errorf("api1 took %s once no link held 10.86.0.1, want it", got)
	}

	want := []string{h.ids["agent1"], h.ids["agent2"], h.ids["agent1"], h.ids["agent1"], h.ids["agent1"], h.ids["agent2"], h.ids["agent2"]}
	if got := h.heard.told(); !slices.Equal(got, want) {
		t.Errorf("the seam heard %v, want every add and remove %v", got, want)
	}
}

func TestRemoveLinkOfATargetTheSandboxDoesNotLinkIsNotFound(t *testing.T) {
	h := newLinkHost(t, "agent1", "web1")

	err := h.svc.RemoveLink(t.Context(), "agent1", "web1")

	var missing *sandbox.LinkNotFoundError
	if !errors.As(err, &missing) || missing.Sandbox != "agent1" || missing.Target != "web1" {
		t.Errorf("remove link = %v, want agent1 holding no link to web1", err)
	}
}

func TestListLinksAnswersThemInTheOrderTheyWereAdded(t *testing.T) {
	h := newLinkHost(t, "agent1", "web1", "db1")
	h.link(t, "agent1", "db1")
	h.link(t, "agent1", "web1")

	got, err := h.svc.ListLinks(t.Context(), "agent1")
	if err != nil {
		t.Fatalf("list links: %v", err)
	}

	if want := h.links(t, "agent1"); !slices.Equal(got, want) || got[0].Target != "db1" {
		t.Errorf("list answered %+v, want db1 then web1 as the record holds them", got)
	}
}

func TestLinksOutliveAStopOfTheTarget(t *testing.T) {
	h := newLinkHost(t, "agent1", "web1")
	link := h.link(t, "agent1", "web1")
	err := h.repo.Update(h.ids["web1"], func(sb *models.Sandbox) error {
		sb.State, sb.PID = models.StateRunning, 42

		return nil
	})
	if err != nil {
		t.Fatalf("run web1: %v", err)
	}
	h.provider.status = models.Status{Exists: true, State: models.StateRunning, PID: 42}

	if _, err := h.svc.Stop(t.Context(), "web1"); err != nil || !h.provider.stopped {
		t.Fatalf("stop = %v with the provider stopped %t, want web1 stopped", err, h.provider.stopped)
	}

	if got := h.links(t, "agent1"); !slices.Equal(got, []models.Link{link}) {
		t.Errorf("after the target stopped agent1 holds %+v, want %+v", got, link)
	}
}

func TestRemoveOfATargetDropsItsLinksFromEverySource(t *testing.T) {
	h := newLinkHost(t, "agent1", "agent2", "web1", "db1")
	h.link(t, "agent1", "web1")
	h.link(t, "agent2", "web1")
	db := h.link(t, "agent1", "db1")
	before := len(h.heard.told())

	if err := h.svc.Remove(t.Context(), "web1", false); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if got := h.links(t, "agent1"); !slices.Equal(got, []models.Link{db}) {
		t.Errorf("agent1 holds %+v, want only its link to db1", got)
	}
	if got := h.links(t, "agent2"); len(got) != 0 {
		t.Errorf("agent2 holds %+v, want no link to a sandbox that is gone", got)
	}
	got, want := h.heard.told()[before:], []string{h.ids["agent1"], h.ids["agent2"]}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the seam heard %v, want both sources that lost a link %v", got, want)
	}
	if got := h.link(t, "agent2", "db1").Address; got != db.Address {
		t.Errorf("agent2 sees db1 at %s, want the address agent1 sees it at, %s", got, db.Address)
	}
}

func TestConcurrentLinksNeverGiveTwoTargetsOneAddress(t *testing.T) {
	const n = 6
	var names []string
	for i := range n {
		names = append(names, fmt.Sprintf("agent%d", i), fmt.Sprintf("web%d", i))
	}
	h := newLinkHost(t, names...)

	var wg sync.WaitGroup
	errs := make(chan error, n*n)
	for i := range n {
		linkEvery(t, h, &wg, errs, i, n)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a concurrent link: %v", err)
		}
	}

	byTarget := map[string]netip.Addr{}
	for i := range n {
		oneAddressEach(t, h.links(t, fmt.Sprintf("agent%d", i)), n, byTarget)
	}
	addresses := map[netip.Addr]bool{}
	for _, a := range byTarget {
		addresses[a] = true
	}
	if len(addresses) != n || !addresses[addr("10.86.0.1")] || !addresses[addr(fmt.Sprintf("10.86.0.%d", n))] {
		t.Errorf("the targets hold %v, want %d addresses from 10.86.0.1 up", byTarget, n)
	}
}

// linkEvery links agent i to every web sandbox at once, each from its own goroutine, in an order of its own.
func linkEvery(t *testing.T, h linkHost, wg *sync.WaitGroup, errs chan<- error, i, n int) {
	for j := range n {
		wg.Go(func() {
			_, err := h.svc.AddLink(t.Context(), fmt.Sprintf("agent%d", i), fmt.Sprintf("web%d", (i+j)%n))
			errs <- err
		})
	}
}

// oneAddressEach checks one source holds n links, each at the address every other source sees its target at.
func oneAddressEach(t *testing.T, links []models.Link, n int, byTarget map[string]netip.Addr) {
	t.Helper()
	if len(links) != n {
		t.Fatalf("a source holds %d links, want %d", len(links), n)
	}
	for _, l := range links {
		if seen, ok := byTarget[l.TargetID]; ok && seen != l.Address {
			t.Errorf("%s is at %s and at %s, want one address", l.Target, seen, l.Address)
		}
		byTarget[l.TargetID] = l.Address
	}
}

// racingRepo deletes a source's record on the first list without the target, as a remove of that source does between the list and the update.
type racingRepo struct {
	*sandboxstate.Repository
	target, source string
	once           sync.Once
	err            error
}

func (r *racingRepo) List() ([]models.Sandbox, error) {
	sandboxes, err := r.Repository.List()
	if err != nil || slices.ContainsFunc(sandboxes, func(sb models.Sandbox) bool { return sb.ID == r.target }) {
		return sandboxes, err
	}
	r.once.Do(func() { r.err = r.Delete(r.source) })

	return sandboxes, nil
}

func TestRemoveOfATargetDropsEveryLinkThoughASourceIsRemovedBesideIt(t *testing.T) {
	h := newLinkHost(t, "agent1", "agent2", "web1")
	h.link(t, "agent1", "web1")
	h.link(t, "agent2", "web1")
	before := len(h.heard.told())
	race := &racingRepo{Repository: h.repo, target: h.ids["web1"], source: h.ids["agent1"]}
	svc, _ := newService(t, &recorder{}, stopped(), func(cfg *sandbox.Config) {
		cfg.Repo, cfg.Links = race, h.heard
	})

	if err := svc.Remove(t.Context(), "web1", false); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := h.repo.Get(h.ids["agent1"]); race.err != nil || !errors.Is(err, sandboxstate.ErrNotFound) {
		t.Fatalf("agent1 = %v after the racing remove (%v), want it gone", err, race.err)
	}

	if got := h.links(t, "agent2"); len(got) != 0 {
		t.Errorf("agent2 holds %+v, want no link to a sandbox that is gone", got)
	}
	if got := h.heard.told()[before:]; !slices.Equal(got, []string{h.ids["agent2"]}) {
		t.Errorf("the seam heard %v, want only agent2 %s", got, h.ids["agent2"])
	}
}

func TestClaimSkipsTheEndsOfTheRangeAndRefusesWhenItIsFull(t *testing.T) {
	taken := map[netip.Addr]bool{}
	for a := addr("10.86.0.1"); a != addr("10.86.255.254"); a = a.Next() {
		taken[a] = true
	}

	last, err := sandbox.ClaimLinkAddress(taken, "web1")
	if err != nil || last != addr("10.86.255.254") {
		t.Fatalf("claim = %s, %v, want 10.86.255.254, the last address below the broadcast", last, err)
	}

	taken[last] = true
	if got, err := sandbox.ClaimLinkAddress(taken, "web2"); err == nil {
		t.Errorf("claim on a full range = %s, want it refused", got)
	}
}

func TestCreateWithLinksRecordsThemAndTellsTheSeam(t *testing.T) {
	h := newLinkHost(t, "agent1", "web1")
	web := h.link(t, "agent1", "web1")
	req := alpine()
	req.Links = []string{"web1"}

	sb, err := h.svc.Create(t.Context(), req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	stored, err := h.repo.Get(sb.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !slices.Equal(stored.Links, []models.Link{web}) {
		t.Errorf("the new sandbox holds %+v, want web1 where agent1 sees it, %+v", stored.Links, web)
	}
	if got := h.heard.told(); !slices.Contains(got, sb.ID) {
		t.Errorf("the seam heard %v, want the new sandbox", got)
	}
}

func TestCreateRefusesLinksBeforeBuildingAnything(t *testing.T) {
	cases := []struct {
		name  string
		links func(web string) []string
	}{
		{"a target that does not exist", func(string) []string { return []string{"ghost"} }},
		{"a target named twice", func(string) []string { return []string{"web1", "web1"} }},
		{"a target named by name and by id", func(web string) []string { return []string{"web1", web} }},
		{"an empty target", func(string) []string { return []string{""} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newLinkHost(t, "web1")
			req := alpine()
			req.Links = tc.links(h.ids["web1"])

			_, err := h.svc.Create(t.Context(), req)

			if !isRequestError(err) {
				t.Fatalf("create = %v, want a request error", err)
			}
			left, err := h.repo.List()
			if err != nil || len(left) != 1 {
				t.Errorf("a refused create left %d records (%v), want web1 alone", len(left), err)
			}
		})
	}
}

func TestForkKeepsTheLinksOfTheSource(t *testing.T) {
	r := &recorder{}
	link := models.Link{Target: "db1", TargetID: "sandbox7", Address: addr("10.86.0.4")}
	source := forkSource()
	source.Links = []models.Link{link}
	told := &heard{}
	svc, l := newService(t, r, source, func(cfg *sandbox.Config) { cfg.Links = told })

	sb, err := svc.Fork(t.Context(), "web", sandbox.CopyRequest{Name: "web-2"})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}

	if !slices.Equal(sb.Links, []models.Link{link}) || !slices.Equal(l.repo.made.Links, []models.Link{link}) {
		t.Errorf("the fork holds %+v, want the source's %+v", sb.Links, link)
	}
	if got := told.told(); !slices.Equal(got, []string{"sandbox2"}) {
		t.Errorf("the seam heard %v, want the fork", got)
	}
}
