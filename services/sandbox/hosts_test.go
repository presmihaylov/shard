package sandbox_test

import (
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/supervisor"
)

const (
	guestHosts  = "127.0.0.1\tlocalhost\n10.200.0.2\tagent1\n"
	hostsBegin  = "# BEGIN shard links: shard rewrites the lines up to the END line as the links change\n"
	hostsEnd    = "# END shard links\n"
	myDBLine    = "10.86.0.1\tmy_db my-db\n"
	webLine     = "10.86.0.2\tweb1\n"
	linkedHosts = guestHosts + hostsBegin + myDBLine + hostsEnd
)

func TestLinkHostsGiveANameWithAnUnderscoreItsDNSSpelling(t *testing.T) {
	link := func(target string) models.Link { return models.Link{Target: target, Address: addr("10.86.0.1")} }
	cases := []struct {
		name   string
		self   string
		target []string
		want   [][]string
	}{
		{"a name with an underscore", "agent1", []string{"my_db"}, [][]string{{"my_db", "my-db"}}},
		{"a name that is already a hostname", "agent1", []string{"web-1"}, [][]string{{"web-1"}}},
		{"an alias another target holds", "agent1", []string{"my_db", "my-db"}, [][]string{{"my_db"}, {"my-db"}}},
		{"an alias the sandbox itself holds", "my-db", []string{"my_db"}, [][]string{{"my_db"}}},
		{"an alias two targets spell alike", "agent1", []string{"a_-b", "a-_b"}, [][]string{{"a_-b"}, {"a-_b"}}},
		{"an alias that starts with a hyphen", "agent1", []string{"_db"}, [][]string{{"_db"}}},
		{"an alias that ends with a hyphen", "agent1", []string{"db_"}, [][]string{{"db_"}}},
		{"an alias over 63 characters", "agent1", []string{strings.Repeat("a", 63) + "_"}, [][]string{{strings.Repeat("a", 63) + "_"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var links []models.Link
			for _, target := range tc.target {
				links = append(links, link(target))
			}

			hosts := sandbox.LinkHosts(tc.self, links)

			var got [][]string
			for _, h := range hosts {
				got = append(got, h.Names)
			}
			if !slices.EqualFunc(got, tc.want, slices.Equal) {
				t.Errorf("names = %v, want %v", got, tc.want)
			}
		})
	}
}

// guestFiles serves each guest's /etc/hosts as shard-init would, and keeps the user every exec ran as.
type guestFiles struct {
	hosts map[string]string
	users []string
}

func (g *guestFiles) serve(t *testing.T, p *fakeProvider) func(models.ExecSpec) (models.ExitStatus, error) {
	return guest(t, func(header supervisor.FileHeader, spec models.ExecSpec) error {
		g.users = append(g.users, spec.User)
		if header.Path != "/etc/hosts" {
			t.Errorf("the guest got %s %s, want /etc/hosts", header.Op, header.Path)
		}
		if header.Op == supervisor.OpPut {
			body := make([]byte, header.Size)
			if _, err := io.ReadFull(spec.Stdin, body); err != nil {
				return err
			}
			g.hosts[p.execID] = string(body)

			return supervisor.WriteMessage(spec.Stdout, supervisor.FileReply{Stat: &models.FileStat{Type: models.FileRegular, Size: header.Size}})
		}
		hosts, ok := g.hosts[p.execID]
		if !ok {
			return supervisor.WriteMessage(spec.Stdout, supervisor.FileReply{Error: "no /etc/hosts", Code: supervisor.FileNotFound})
		}
		if err := supervisor.WriteMessage(spec.Stdout, supervisor.FileReply{Stat: &models.FileStat{Type: models.FileRegular, Size: int64(len(hosts))}}); err != nil {
			return err
		}
		_, err := io.WriteString(spec.Stdout, hosts)

		return err
	})
}

// runningLinkHost is a link host whose named sandboxes run, each guest holding guestHosts.
func runningLinkHost(t *testing.T, names []string, running ...string) (linkHost, *guestFiles) {
	t.Helper()

	h := newLinkHost(t, names...)
	g := &guestFiles{hosts: map[string]string{}}
	for _, name := range running {
		h.setState(t, name, models.StateRunning)
		g.hosts[h.ids[name]] = guestHosts
	}
	h.provider.status = models.Status{Exists: true, State: models.StateRunning, PID: 42}
	h.provider.serve = g.serve(t, h.provider)

	return h, g
}

func (h linkHost) setState(t *testing.T, name string, state models.State) {
	t.Helper()

	err := h.repo.Update(h.ids[name], func(sb *models.Sandbox) error {
		sb.State, sb.PID = state, 42
		if state == models.StatePaused {
			sb.PID, sb.Checkpoint = 0, "/checkpoints/"+name
		}

		return nil
	})
	if err != nil {
		t.Fatalf("set %s %s: %v", name, state, err)
	}
}

func TestLinksOfARunningSourceReachItsHostsAsRoot(t *testing.T) {
	h, g := runningLinkHost(t, []string{"agent1", "my_db", "web1"}, "agent1")

	h.link(t, "agent1", "my_db")
	if got := g.hosts[h.ids["agent1"]]; got != linkedHosts {
		t.Fatalf("after the link agent1's hosts = %q, want %q", got, linkedHosts)
	}
	h.link(t, "agent1", "web1")
	if got, want := g.hosts[h.ids["agent1"]], guestHosts+hostsBegin+myDBLine+webLine+hostsEnd; got != want {
		t.Fatalf("after the second link agent1's hosts = %q, want %q", got, want)
	}

	if err := h.svc.RemoveLink(t.Context(), "agent1", "my_db"); err != nil {
		t.Fatalf("remove link: %v", err)
	}
	if got, want := g.hosts[h.ids["agent1"]], guestHosts+hostsBegin+webLine+hostsEnd; got != want {
		t.Fatalf("after the unlink agent1's hosts = %q, want %q", got, want)
	}
	if err := h.svc.RemoveLink(t.Context(), "agent1", "web1"); err != nil {
		t.Fatalf("remove link: %v", err)
	}
	if got := g.hosts[h.ids["agent1"]]; got != guestHosts {
		t.Fatalf("with no link agent1's hosts = %q, want the guest's own %q", got, guestHosts)
	}
	if i := slices.IndexFunc(g.users, func(u string) bool { return u != "0" }); i >= 0 {
		t.Errorf("an exec ran as %q, want root for the file root owns", g.users[i])
	}
}

func TestALinkOfAStoppedSourceExecsNothing(t *testing.T) {
	h, g := runningLinkHost(t, []string{"agent1", "my_db"})

	h.link(t, "agent1", "my_db")

	if len(g.users) != 0 {
		t.Errorf("the link ran %d execs in a stopped source, want none", len(g.users))
	}
}

func TestRemoveOfATargetTakesItsLineOutOfEveryRunningSource(t *testing.T) {
	h, g := runningLinkHost(t, []string{"agent1", "agent2", "my_db", "web1"}, "agent1", "agent2")
	h.link(t, "agent1", "my_db")
	h.link(t, "agent1", "web1")
	h.link(t, "agent2", "web1")
	// One status answers for every sandbox: web1 is down as the remove asks, and the sources are up as their writes ask.
	live := h.provider.status
	h.provider.status = models.Status{Exists: true, State: models.StateStopped}
	h.provider.onRemove = func() { h.provider.status = live }

	if err := h.svc.Remove(t.Context(), "web1", false); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if got := g.hosts[h.ids["agent1"]]; got != linkedHosts {
		t.Errorf("agent1's hosts = %q, want my_db alone %q", got, linkedHosts)
	}
	if got := g.hosts[h.ids["agent2"]]; got != guestHosts {
		t.Errorf("agent2's hosts = %q, want the guest's own %q", got, guestHosts)
	}
}

func TestAHostsWriteThatFailsKeepsTheLink(t *testing.T) {
	h, _ := runningLinkHost(t, []string{"agent1", "my_db"}, "agent1")
	h.provider.serve = guest(t, refuse(""))

	_, err := h.svc.AddLink(t.Context(), "agent1", "my_db")

	if err == nil || !strings.Contains(err.Error(), "/etc/hosts") {
		t.Fatalf("link = %v, want the hosts write's failure", err)
	}
	if got := h.links(t, "agent1"); len(got) != 1 {
		t.Errorf("agent1 holds %+v, want the link the seam serves", got)
	}
}

func TestStartAndResumeWriteTheLinksTheGuestMissed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state models.State
		verb  func(*sandbox.Service, *testing.T) (models.Sandbox, error)
	}{
		{"start", models.StateStopped, func(s *sandbox.Service, t *testing.T) (models.Sandbox, error) { return s.Start(t.Context(), "agent1") }},
		{"resume", models.StatePaused, func(s *sandbox.Service, t *testing.T) (models.Sandbox, error) { return s.Resume(t.Context(), "agent1") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, g := runningLinkHost(t, []string{"agent1", "my_db"})
			h.link(t, "agent1", "my_db")
			h.setState(t, "agent1", tc.state)
			g.hosts[h.ids["agent1"]] = guestHosts

			if _, err := tc.verb(h.svc, t); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}

			if got := g.hosts[h.ids["agent1"]]; got != linkedHosts {
				t.Errorf("after the %s agent1's hosts = %q, want %q", tc.name, got, linkedHosts)
			}
		})
	}
}

func TestAStartWhoseHostsWriteFailsRunsOnAndReportsIt(t *testing.T) {
	h, _ := runningLinkHost(t, []string{"agent1", "my_db"})
	h.link(t, "agent1", "my_db")
	h.provider.serve = guest(t, refuse(""))

	sb, err := h.svc.Start(t.Context(), "agent1")

	if err != nil || sb.State != models.StateRunning {
		t.Fatalf("start = %s, %v, want it running", sb.State, err)
	}
	if got := *h.reports; len(got) != 1 || !strings.Contains(got[0], "did not reach its /etc/hosts") {
		t.Errorf("the service reported %q, want the hosts write's failure", got)
	}
}

func TestAStartWithNoLinksExecsNothing(t *testing.T) {
	h, g := runningLinkHost(t, []string{"agent1"})

	if _, err := h.svc.Start(t.Context(), "agent1"); err != nil {
		t.Fatalf("start: %v", err)
	}

	if len(g.users) != 0 {
		t.Errorf("the start ran %d execs, want none for a sandbox with no links", len(g.users))
	}
}

func TestCreateWithLinksHandsTheProviderTheirHosts(t *testing.T) {
	h := newLinkHost(t, "my_db")
	req := alpine()
	req.Links = []string{"my_db"}

	if _, err := h.svc.Create(t.Context(), req); err != nil {
		t.Fatalf("create: %v", err)
	}

	want := []models.Host{{Address: addr("10.86.0.1"), Names: []string{"my_db", "my-db"}}}
	if got := h.provider.spec.Network.Hosts; !slices.EqualFunc(got, want, sameHost) {
		t.Errorf("the provider got hosts %+v, want %+v", got, want)
	}
}

func sameHost(a, b models.Host) bool { return a.Address == b.Address && slices.Equal(a.Names, b.Names) }
