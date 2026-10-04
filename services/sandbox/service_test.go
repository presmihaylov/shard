package sandbox_test

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/network"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

func alpine() sandbox.CreateRequest {
	return sandbox.CreateRequest{Image: "alpine:3.20", Command: []string{"echo", "1"}}
}

func TestCreateAnswersTheRecordAndTearsNothingDown(t *testing.T) {
	r := &recorder{}
	svc, _ := newService(t, r, models.Sandbox{})

	sb, err := svc.Create(t.Context(), alpine())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if sb.ID != "sandbox1" || sb.State != models.StateRunning {
		t.Errorf("create answered %+v, want sandbox1 running", sb)
	}

	for _, call := range r.calls {
		if call == "repo.Delete" || call == "net.Release" || call == "provider.Remove" {
			t.Errorf("a successful create tore down %s; the sandbox outlives the verb", call)
		}
	}
}

// ls counts UPTIME from StartedAt, so the first start records it like every later one.
func TestCreateRecordsWhenTheSandboxStarted(t *testing.T) {
	r := &recorder{}
	svc, _ := newService(t, r, models.Sandbox{})

	before := time.Now()
	sb, err := svc.Create(t.Context(), alpine())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if sb.StartedAt.Before(before) {
		t.Errorf("create recorded StartedAt %v, want at or after %v", sb.StartedAt, before)
	}
}

// Create reports whether the create succeeded, and it never waits for a process a sandbox may outlive.
func TestCreateNeverWaitsForTheEntrypoint(t *testing.T) {
	r := &recorder{}
	svc, _ := newService(t, r, models.Sandbox{})

	if _, err := svc.Create(t.Context(), alpine()); err != nil {
		t.Fatalf("create: %v", err)
	}

	if slices.Contains(r.calls, "provider.Wait") {
		t.Errorf("create waited for the entrypoint: %v", r.calls)
	}
}

// TestCreateTearsDownWhatItBuilt forces a failure at each claim and asserts exactly what is given back.
// A failed create keeps the record and marks it failed, so it never gives the record back.
func TestCreateTearsDownWhatItBuilt(t *testing.T) {
	cases := []struct {
		failAt   string
		giveBack []string
	}{
		{"images.Pull", nil},
		{"repo.Dir", nil},
		{"net.Allocate", []string{"net.Release"}},
		// Create rolls back its own mount only, so the substrate claim is given back here too.
		{"provider.Create", []string{"provider.Remove", "net.Release"}},
		{"provider.Status", []string{"provider.Remove", "net.Release"}},
		{"repo.Update#1", []string{"provider.Remove", "net.Release"}},
		{"provider.Start", []string{"provider.Remove", "net.Release"}},
	}

	for _, c := range cases {
		t.Run(c.failAt, func(t *testing.T) {
			r := &recorder{fail: []string{c.failAt}}
			svc, l := newService(t, r, models.Sandbox{})

			if _, err := svc.Create(t.Context(), alpine()); err == nil {
				t.Fatal("a forced failure returned no error")
			}

			if got := keep(r.calls, "provider.Remove", "net.Release"); !slices.Equal(got, c.giveBack) {
				t.Errorf("gave back %v, want %v", got, c.giveBack)
			}

			// The record is the failure report, so it stays: only rm frees a failed sandbox.
			if slices.Contains(r.calls, "repo.Delete") {
				t.Errorf("a failed create deleted the record: %v", r.calls)
			}
			if l.repo.sb.State != models.StateFailed || l.repo.sb.FailedReason == "" {
				t.Errorf("the record is %s reason %q, want failed with a reason", l.repo.sb.State, l.repo.sb.FailedReason)
			}
		})
	}
}

// A bound the substrate refuses is a bad request: nothing was pulled or claimed, so no record may say failed.
func TestCreateRefusedByTheProviderLeavesNoRecord(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, models.Sandbox{})
	l.provider.refuse = errors.New("provider fake takes no --memory 0")

	_, err := svc.Create(t.Context(), alpine())

	var refused *sandbox.RequestError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "--memory 0") {
		t.Fatalf("create = %v, want a request error with the provider's reason", err)
	}
	if slices.Contains(r.calls, "repo.Create") || slices.Contains(r.calls, "images.Pull") {
		t.Errorf("a refused create reached the store: %v", r.calls)
	}
	if l.repo.sb.ID != "" {
		t.Errorf("a refused create left the record %+v", l.repo.sb)
	}
}

// diskProvider is a VM substrate, which admits the disk of a create before its record exists.
type diskProvider struct {
	models.Provider

	refuse   error
	admitted []string
	released []string
}

func (d *diskProvider) AdmitDisk(dir string, _ models.Resources) error {
	if d.refuse != nil {
		return d.refuse
	}
	d.admitted = append(d.admitted, dir)

	return nil
}

func (d *diskProvider) ReleaseDisk(dir string) { d.released = append(d.released, dir) }

func withDisks(d *diskProvider) func(*sandbox.Config) {
	return func(c *sandbox.Config) {
		d.Provider = c.Provider
		c.Provider = d
	}
}

// A root the admission could not read is the daemon's fault, so the create is no bad request.
func TestCreateWhoseRootCannotBeReadIsNoBadRequest(t *testing.T) {
	disks := &diskProvider{refuse: fmt.Errorf("statfs /var/lib/shard: %w", os.ErrPermission)}
	svc, _ := newService(t, &recorder{}, models.Sandbox{}, withDisks(disks))

	_, err := svc.Create(t.Context(), alpine())
	var request *sandbox.RequestError
	if err == nil || errors.As(err, &request) {
		t.Fatalf("create = %v, want a failure that is not the request's fault", err)
	}
	if public, ok := sandbox.PublicText(err); ok {
		t.Errorf("public text = %q, want none for a host failure", public)
	}
}

// A disk the root has no room for is refused before the record, so no verb ever sees the sandbox (SHARD-393).
func TestCreateRefusedByTheDiskAdmissionLeavesNoRecord(t *testing.T) {
	r := &recorder{}
	disks := &diskProvider{refuse: &bundle.NoRoomError{Bound: 4096 << 20}}
	svc, l := newService(t, r, models.Sandbox{}, withDisks(disks))

	_, err := svc.Create(t.Context(), alpine())

	var refused *sandbox.RequestError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "does not fit on the root") {
		t.Fatalf("create = %v, want a request error with the admission's reason", err)
	}
	if slices.Contains(r.calls, "repo.Create") || slices.Contains(r.calls, "images.Pull") {
		t.Errorf("a refused create reached the store: %v", r.calls)
	}
	if l.repo.sb.ID != "" {
		t.Errorf("a refused create left the record %+v", l.repo.sb)
	}
}

// The record write can fail after the admission, and nothing will write that disk then.
func TestAnAdmittedDiskIsReleasedWhenTheRecordFails(t *testing.T) {
	r := &recorder{fail: []string{"repo.Create"}}
	disks := &diskProvider{}
	svc, _ := newService(t, r, models.Sandbox{}, withDisks(disks))

	if _, err := svc.Create(t.Context(), alpine()); err == nil {
		t.Fatal("create succeeded over a record write that failed")
	}
	if want := []string{"/sandboxes/sandbox1"}; !slices.Equal(disks.released, want) {
		t.Errorf("released %v, want %v", disks.released, want)
	}
}

func TestACreateKeepsTheDiskItWasAdmitted(t *testing.T) {
	r := &recorder{}
	disks := &diskProvider{}
	svc, _ := newService(t, r, models.Sandbox{}, withDisks(disks))

	if _, err := svc.Create(t.Context(), alpine()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"/sandboxes/sandbox1"}; !slices.Equal(disks.admitted, want) {
		t.Errorf("admitted %v, want %v", disks.admitted, want)
	}
	if len(disks.released) != 0 {
		t.Errorf("a create that took released its disk: %v", disks.released)
	}
}

// A bound past the host's memory never binds, so it is refused by name before anything is pulled or recorded.
func TestCreateRefusesMoreMemoryThanTheHostHas(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, models.Sandbox{}, func(c *sandbox.Config) { c.HostMemoryMiB = 4096 })
	req := alpine()
	req.Resources.MemoryMiB = new(int64(4097))

	_, err := svc.Create(t.Context(), req)

	var refused *sandbox.RequestError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "--memory 4097MiB is more than the 4096 MiB") {
		t.Fatalf("create = %v, want a request error that names the bound and the host", err)
	}
	if slices.Contains(r.calls, "repo.Create") || slices.Contains(r.calls, "images.Pull") {
		t.Errorf("a refused create reached the store: %v", r.calls)
	}
	if l.repo.sb.ID != "" {
		t.Errorf("a refused create left the record %+v", l.repo.sb)
	}
}

func TestCreateTakesTheWholeHostMemory(t *testing.T) {
	r := &recorder{}
	svc, _ := newService(t, r, models.Sandbox{}, func(c *sandbox.Config) { c.HostMemoryMiB = 4096 })
	req := alpine()
	req.Resources.MemoryMiB = new(int64(4096))

	if _, err := svc.Create(t.Context(), req); err != nil {
		t.Fatalf("create with the host's whole memory: %v", err)
	}
}

// A quota past the host's CPUs never binds, and a large one wraps to no bound, so both are refused by name.
func TestCreateRefusesMoreCPUsThanTheHostHas(t *testing.T) {
	for _, cpus := range []int{9, 92233720368548} {
		r := &recorder{}
		svc, l := newService(t, r, models.Sandbox{}, func(c *sandbox.Config) { c.HostCPUs = 8 })
		req := alpine()
		req.Resources.VCPUs = cpus

		_, err := svc.Create(t.Context(), req)

		var refused *sandbox.RequestError
		if want := fmt.Sprintf("--cpus %d is more than the 8 CPUs this host has", cpus); !errors.As(err, &refused) || !strings.Contains(err.Error(), want) {
			t.Fatalf("create with %d cpus = %v, want a request error that says %q", cpus, err, want)
		}
		if slices.Contains(r.calls, "repo.Create") || slices.Contains(r.calls, "images.Pull") {
			t.Errorf("a refused create reached the store: %v", r.calls)
		}
		if l.repo.sb.ID != "" {
			t.Errorf("a refused create left the record %+v", l.repo.sb)
		}
	}
}

func TestCreateTakesEveryHostCPU(t *testing.T) {
	svc, _ := newService(t, &recorder{}, models.Sandbox{}, func(c *sandbox.Config) { c.HostCPUs = 8 })
	req := alpine()
	req.Resources.VCPUs = 8

	if _, err := svc.Create(t.Context(), req); err != nil {
		t.Fatalf("create with every host cpu: %v", err)
	}
}

// A cancelled context would fail every give-back at once, so the unwind builds its own.
func TestCreateTearsDownAfterAnInterrupt(t *testing.T) {
	r := &recorder{fail: []string{"provider.Create"}}
	svc, _ := newService(t, r, models.Sandbox{})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := svc.Create(ctx, alpine()); err == nil {
		t.Fatal("a forced failure returned no error")
	}

	for _, step := range []string{"provider.Remove", "net.Release"} {
		if !r.live[step] {
			t.Errorf("%s ran on the cancelled context, so it could not give anything back", step)
		}
	}
}

// The record write after a successful start is the one place the keep-alive rule nearly broke.
func TestCreateKeepsALiveSandboxWhenTheRecordWriteFails(t *testing.T) {
	r := &recorder{fail: []string{"repo.Update#2"}}
	svc, _ := newService(t, r, models.Sandbox{})

	_, err := svc.Create(t.Context(), alpine())
	if err == nil || !strings.Contains(err.Error(), "sandbox sandbox1 is running") {
		t.Fatalf("create = %v, want the live sandbox named", err)
	}

	for _, step := range []string{"provider.Remove", "net.Release", "repo.Delete"} {
		if slices.Contains(r.calls, step) {
			t.Errorf("ran %s on a started sandbox; only stop ends one", step)
		}
	}
}

// The stack is LIFO, so a step that failed still holds what the steps below it name.
func TestCreateStopsUnwindingAtTheFirstFailure(t *testing.T) {
	r := &recorder{fail: []string{"provider.Start", "provider.Remove"}}
	svc, _ := newService(t, r, models.Sandbox{})

	if _, err := svc.Create(t.Context(), alpine()); err == nil {
		t.Fatal("a forced failure returned no error")
	}

	// The unwind is LIFO and stops at the first give-back that fails, so net.Release never runs.
	if got := keep(r.calls, "provider.Remove", "net.Release", "repo.Delete"); !slices.Equal(got, []string{"provider.Remove"}) {
		t.Errorf("unwound %v, want it to stop at provider.Remove", got)
	}
}

// runsc reports the cancellation, not what it did, so a force-delete here could end a live guest.
func TestCreateKeepsASandboxAnInterruptedStartMayHaveStarted(t *testing.T) {
	r := &recorder{fail: []string{"provider.Start"}}
	svc, _ := newService(t, r, models.Sandbox{})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := svc.Create(ctx, alpine())
	if err == nil || !strings.Contains(err.Error(), "sandbox1") {
		t.Fatalf("create = %v, want the kept sandbox named", err)
	}

	for _, step := range []string{"provider.Remove", "net.Release", "repo.Delete"} {
		if slices.Contains(r.calls, step) {
			t.Errorf("ran %s after an interrupted start; the entrypoint may already be live", step)
		}
	}
}

// A later process reaches the sandbox through the record alone, so what the substrate decided lands in it.
func TestCreateRecordsWhatTheSubstrateDecided(t *testing.T) {
	svc, l := newService(t, &recorder{fail: []string{"repo.Update#2"}}, models.Sandbox{})

	if _, err := svc.Create(t.Context(), alpine()); err == nil {
		t.Fatal("a forced failure returned no error")
	}

	if l.repo.sb.PID != 42 {
		t.Errorf("the record holds pid %d, want the one the substrate reported", l.repo.sb.PID)
	}
	if l.repo.sb.HostInterface != "shardv2" {
		t.Errorf("the record holds host interface %q, want shardv2", l.repo.sb.HostInterface)
	}
	if l.repo.sb.Digest != fakeDigest {
		t.Errorf("the record holds digest %q, want the pulled image's %s", l.repo.sb.Digest, fakeDigest)
	}
	if l.repo.sb.State != models.StatePending {
		t.Errorf("the record says %s before the start was recorded, want pending", l.repo.sb.State)
	}
}

// The pool is the one thing nothing frees on a timer, so its refusal names the verbs that do.
func TestCreateNamesLsWhenNoAddressIsFree(t *testing.T) {
	svc, l := newService(t, &recorder{}, models.Sandbox{})
	l.net.allocateErr = network.ErrNoFreeAddress

	_, err := svc.Create(t.Context(), alpine())
	if !errors.Is(err, network.ErrNoFreeAddress) || !strings.Contains(err.Error(), "shard list --all") {
		t.Errorf("create failed with %v, want the pool's refusal naming shard list --all", err)
	}
}

func TestCreateRefusesWhatNoStoreCouldHold(t *testing.T) {
	cases := map[string]sandbox.CreateRequest{
		"no image":                {},
		"a name no verb takes":    {Image: "alpine", Name: "a/b"},
		"a negative memory":       {Image: "alpine", Resources: sandbox.ResourceRequest{MemoryMiB: new(int64(-512))}},
		"a memory that overflows": {Image: "alpine", Resources: sandbox.ResourceRequest{MemoryMiB: new(int64(sandbox.MaxMemoryMiB + 1))}},
		"a negative cpu bound":    {Image: "alpine", Resources: sandbox.ResourceRequest{VCPUs: -2}},
		"a negative disk bound":   {Image: "alpine", Resources: sandbox.ResourceRequest{DiskMiB: -1}},
		"a disk that overflows":   {Image: "alpine", Resources: sandbox.ResourceRequest{DiskMiB: sandbox.MaxDiskMiB + 1}},
		"a bad policy name":       {Image: "alpine", Policy: "Bad Name"},
		"an env with no value":    {Image: "alpine", Env: []string{"DEBUG"}},
		"an env with no name":     {Image: "alpine", Env: []string{"=1"}},
		"a bad secret name":       {Image: "alpine", Secrets: []string{"api_key"}},
		"a doubled secret":        {Image: "alpine", Secrets: []string{"KEY", "KEY"}},
		"a secret an env shadows": {Image: "alpine", Secrets: []string{"KEY"}, Env: []string{"KEY=1"}},
	}

	for name, req := range cases {
		r := &recorder{}
		svc, _ := newService(t, r, models.Sandbox{})

		_, err := svc.Create(t.Context(), req)
		if err == nil {
			t.Errorf("create(%s) returned no error", name)
		}
		if len(r.calls) > 0 {
			t.Errorf("create(%s) reached a layer before the refusal: %v", name, r.calls)
		}
	}
}

func TestCreateHandsTheGuestThePlaceholderAndRecordsTheGrant(t *testing.T) {
	svc, l := newService(t, &recorder{}, models.Sandbox{})

	if _, err := l.secrets.Set("API_KEY", "sk-live-1234567890", []string{"api.example.com"}, ""); err != nil {
		t.Fatal(err)
	}

	req := alpine()
	req.Secrets = []string{"API_KEY"}
	req.Env = []string{"OTHER=1"}

	if _, err := svc.Create(t.Context(), req); err != nil {
		t.Fatalf("create: %v", err)
	}

	if !slices.Contains(l.provider.spec.Env, "API_KEY=mock-API_KEY") {
		t.Errorf("the guest env %v holds no placeholder", l.provider.spec.Env)
	}
	if slices.ContainsFunc(l.provider.spec.Env, func(e string) bool { return strings.Contains(e, "sk-live") }) {
		t.Fatalf("the guest env %v holds the value", l.provider.spec.Env)
	}

	if strings.Join(l.repo.created.Secrets, ",") != "API_KEY" {
		t.Errorf("the record grants %v, want API_KEY", l.repo.created.Secrets)
	}

	// The guest reaches the granted host through the proxy, so it must trust the CA the proxy terminates with.
	if !strings.HasPrefix(string(l.provider.spec.ProxyCA), "-----BEGIN CERTIFICATE-----") {
		t.Errorf("the spec carries %q as the proxy CA", l.provider.spec.ProxyCA)
	}
}

// An unfronted sandbox reaches the network directly, so nothing plants a CA in its trust store.
func TestCreateWithoutAPolicyOrASecretFrontsNothing(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, models.Sandbox{})

	if _, err := svc.Create(t.Context(), alpine()); err != nil {
		t.Fatalf("create: %v", err)
	}

	if l.provider.spec.ProxyCA != nil {
		t.Error("an unfronted sandbox was handed the proxy CA")
	}
	if slices.Contains(r.calls, "net.Reapply") {
		t.Errorf("a create with no policy and no secret reapplied the rules: %v", r.calls)
	}
}

// shard owns the trust store of a fronted sandbox, so an env that points a client elsewhere is refused.
func TestCreateRefusesATrustVariableOnAFrontedSandbox(t *testing.T) {
	r := &recorder{}
	svc, _ := newService(t, r, models.Sandbox{})

	req := alpine()
	req.Env = []string{"SSL_CERT_FILE=/mine.pem"}
	if _, err := svc.Create(t.Context(), req); err != nil {
		t.Fatalf("an unfronted create refused SSL_CERT_FILE: %v", err)
	}

	req.Secrets = []string{"API_KEY"}

	_, err := svc.Create(t.Context(), req)

	var refused *sandbox.RequestError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "SSL_CERT_FILE") {
		t.Fatalf("a fronted create with SSL_CERT_FILE = %v, want a request error naming it", err)
	}
}

func TestCreateRefusesASecretTheStoreDoesNotHoldBeforeThePull(t *testing.T) {
	r := &recorder{}
	svc, _ := newService(t, r, models.Sandbox{})

	req := alpine()
	req.Secrets = []string{"NOPE"}

	_, err := svc.Create(t.Context(), req)

	var refused *sandbox.RequestError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "secret NOPE does not exist") {
		t.Fatalf("create = %v, want a request error naming the secret", err)
	}
	if slices.Contains(r.calls, "images.Pull") {
		t.Errorf("a missing secret still cost a pull: %v", r.calls)
	}
}

// The proxy sets the trust variables itself, so a secret of that name is refused at create as at a grant.
func TestCreateRefusesASecretNamedForATrustVariableBeforeThePull(t *testing.T) {
	for _, name := range bundle.TrustEnv {
		t.Run(name, func(t *testing.T) {
			r := &recorder{}
			svc, l := newService(t, r, models.Sandbox{})
			if _, err := l.secrets.Set(name, "s3cr3t", []string{"api.example.com"}, ""); err != nil {
				t.Fatalf("secrets.Set: %v", err)
			}

			req := alpine()
			req.Secrets = []string{name}

			_, err := svc.Create(t.Context(), req)

			var refused *sandbox.RequestError
			if !errors.As(err, &refused) || !strings.Contains(err.Error(), "trust store") {
				t.Fatalf("create = %v, want a request error that names the trust store", err)
			}
			if slices.Contains(r.calls, "images.Pull") {
				t.Errorf("a refused secret still cost a pull: %v", r.calls)
			}
		})
	}
}

func TestCreateWithAPolicyTellsTheHostBeforeTheStart(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, models.Sandbox{})

	if err := l.policies.Set(models.Policy{Name: "locked"}); err != nil {
		t.Fatal(err)
	}

	req := alpine()
	req.Policy = "locked"

	if _, err := svc.Create(t.Context(), req); err != nil {
		t.Fatalf("create: %v", err)
	}

	want := []string{"net.Allocate", "provider.Create", "net.Reapply", "provider.Start"}
	if got := keep(r.calls, want...); !slices.Equal(got, want) {
		t.Errorf("the network was driven as %v, want %v", got, want)
	}
	if got := l.repo.created.Policy; got != "locked" {
		t.Errorf("the record names policy %q, want locked", got)
	}
}

func TestCreateRefusesAPolicyTheStoreDoesNotHoldBeforeThePull(t *testing.T) {
	r := &recorder{}
	svc, _ := newService(t, r, models.Sandbox{})

	req := alpine()
	req.Policy = "ghost"

	_, err := svc.Create(t.Context(), req)

	var refused *sandbox.RequestError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "policy not found") {
		t.Fatalf("create = %v, want a request error naming the policy", err)
	}
	if slices.Contains(r.calls, "images.Pull") {
		t.Errorf("a missing policy still cost a pull: %v", r.calls)
	}
}

func TestCreateWithoutAPolicyNeverReappliesTheRules(t *testing.T) {
	r := &recorder{}
	svc, _ := newService(t, r, models.Sandbox{})

	if _, err := svc.Create(t.Context(), alpine()); err != nil {
		t.Fatalf("create: %v", err)
	}
	if slices.Contains(r.calls, "net.Reapply") {
		t.Errorf("a create with no policy reapplied the rules: %v", r.calls)
	}
}

// A policy sandbox may ask only shard's resolver, and one without keeps the public nameservers, secret or not.
func TestCreateTurnsAPolicySandboxsLookupsToTheGateway(t *testing.T) {
	gateway := []netip.Addr{netip.MustParseAddr("10.0.0.1")}
	public := []netip.Addr{netip.MustParseAddr("1.1.1.1")}

	for name, tc := range map[string]struct {
		policy string
		secret string
		want   []netip.Addr
	}{
		"a policy":      {policy: "locked", want: gateway},
		"no policy":     {want: public},
		"a secret only": {secret: "API_KEY", want: public},
	} {
		t.Run(name, func(t *testing.T) {
			svc, l := newService(t, &recorder{}, models.Sandbox{})
			if err := l.policies.Set(models.Policy{Name: "locked"}); err != nil {
				t.Fatal(err)
			}
			if _, err := l.secrets.Set("API_KEY", "sk-live-1234567890", []string{"api.example.com"}, ""); err != nil {
				t.Fatal(err)
			}

			req := alpine()
			req.Policy = tc.policy
			if tc.secret != "" {
				req.Secrets = []string{tc.secret}
			}
			if _, err := svc.Create(t.Context(), req); err != nil {
				t.Fatalf("create: %v", err)
			}
			if got := l.provider.spec.Network.Nameservers; !slices.Equal(got, tc.want) {
				t.Errorf("the guest resolves through %v, want %v", got, tc.want)
			}
		})
	}
}

// The record carries the name so a later verb resolves it, and the spec so the guest hostname is it.
func TestCreateGivesTheNameToTheRecordAndToTheGuest(t *testing.T) {
	svc, l := newService(t, &recorder{}, models.Sandbox{})

	req := alpine()
	req.Name = "builder"

	if _, err := svc.Create(t.Context(), req); err != nil {
		t.Fatalf("create: %v", err)
	}

	if got := l.repo.created.Name; got != "builder" {
		t.Fatalf("the record carries the name %q, want builder", got)
	}
	if got := l.provider.spec.Name; got != "builder" {
		t.Fatalf("the spec carries the name %q, want builder", got)
	}
}

func TestCreateWithoutANameGivesTheGuestTheID(t *testing.T) {
	svc, l := newService(t, &recorder{}, models.Sandbox{})

	if _, err := svc.Create(t.Context(), alpine()); err != nil {
		t.Fatalf("create: %v", err)
	}

	if got := l.repo.created.Name; got != "" {
		t.Fatalf("an unnamed create put %q in the record", got)
	}
	if l.provider.spec.Name != l.provider.spec.ID {
		t.Fatalf("the guest hostname is %q, want the id %q", l.provider.spec.Name, l.provider.spec.ID)
	}
}

func TestStartRunsAStoppedSandboxAgain(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, stopped())

	sb, err := svc.Start(t.Context(), "web")
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	if sb.ID != "sandbox1" {
		t.Errorf("start answered %q, want the id", sb.ID)
	}
	if !l.provider.started {
		t.Error("the provider was never asked to start")
	}

	// gVisor took the guest address at the first create, so the netns is built again before the run.
	if !l.net.allocated || slices.Index(r.calls, "net.Allocate") > slices.Index(r.calls, "provider.Start") {
		t.Errorf("the network was not built again before the start: %v", r.calls)
	}

	if sb.State != models.StateRunning || sb.PID != 7 {
		t.Errorf("the record is %s with pid %d, want running with pid 7", sb.State, sb.PID)
	}
	if sb.ExitStatus != nil {
		t.Errorf("the record still holds the old exit %+v", *sb.ExitStatus)
	}

	// The record says running only once the sandbox is, so a failed start leaves it stopped.
	if slices.Index(r.calls, "provider.Start") > slices.Index(r.calls, "repo.Update") {
		t.Errorf("the record was updated before the start: %v", r.calls)
	}
}

func TestStartRefusesASandboxThatIsNotStopped(t *testing.T) {
	for _, state := range []models.State{models.StateRunning, models.StateCreated} {
		sb := running()
		sb.State = state
		svc, l := newService(t, &recorder{}, sb)

		_, err := svc.Start(t.Context(), "sandbox1")

		var refused *sandbox.StateError
		if !errors.As(err, &refused) || refused.State != state {
			t.Errorf("start of a %s sandbox returned %v, want a state error naming the state", state, err)
		}
		if l.provider.started {
			t.Errorf("start of a %s sandbox reached the provider", state)
		}
	}
}

// An unresponsive record refuses a start with the reason it holds, as docs/state-machine.md promises (SHARD-424).
func TestStartRefusesAnUnresponsiveSandboxWithItsReason(t *testing.T) {
	svc, l := newService(t, &recorder{}, unresponsive())

	_, err := svc.Start(t.Context(), "sandbox1")
	if err == nil || !strings.Contains(err.Error(), "is unresponsive: "+silentShim().Reason+": start takes a stopped sandbox") {
		t.Errorf("start of an unresponsive sandbox returned %v, want the refusal with the reason", err)
	}
	if l.provider.started {
		t.Error("start of an unresponsive sandbox reached the provider")
	}
}

// A mark an unfinished pause left would vouch for that pause's checkpoint in the new run, so a start drops it.
func TestStartDropsTheMarkOfAnUnfinishedPause(t *testing.T) {
	sb := stopped()
	sb.Pausing = true
	svc, _ := newService(t, &recorder{}, sb)

	got, err := svc.Start(t.Context(), "web")
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	if got.State != models.StateRunning || got.Pausing {
		t.Errorf("the record is %s with mark %v, want running with none", got.State, got.Pausing)
	}
}

func TestStartKeepsTheRecordStoppedWhenTheProviderFails(t *testing.T) {
	svc, l := newService(t, &recorder{fail: []string{"provider.Start"}}, stopped())

	if _, err := svc.Start(t.Context(), "sandbox1"); err == nil {
		t.Fatal("start returned no error")
	}

	if sb := l.repo.sb; sb.State != models.StateStopped || sb.ExitStatus == nil {
		t.Errorf("the record is %s with exit %v after a failed start, want stopped with its exit kept", sb.State, sb.ExitStatus)
	}
}

// A start that failed after the substrate came up leaves a live sandbox, and only stop ends one.
func TestStartRecordsASandboxThatCameUpUnderAFailedStart(t *testing.T) {
	svc, l := newService(t, &recorder{fail: []string{"provider.Start"}}, stopped())
	l.provider.status = models.Status{Exists: true, State: models.StateRunning, PID: 9}

	_, err := svc.Start(t.Context(), "sandbox1")
	if err == nil || !strings.Contains(err.Error(), "may be running") {
		t.Fatalf("start returned %v, want the failure and the warning that the sandbox stays", err)
	}

	if sb := l.repo.sb; sb.State != models.StateRunning || sb.PID != 9 {
		t.Errorf("the record is %s with pid %d, want running with pid 9", sb.State, sb.PID)
	}
}

// A shard-init that died at boot leaves a stopped sandbox, so the start lands its exit and its reason (SHARD-416).
func TestStartRecordsTheExitAndTheReasonOfAShardInitThatDiedAtBoot(t *testing.T) {
	svc, l := newService(t, &recorder{fail: []string{"provider.Start"}}, stopped())
	why := "mount /dev/vdb on /overlay: read-only file system"
	l.provider.status = models.Status{Exists: true, State: models.StateStopped, SupervisorFailed: why}

	if _, err := svc.Start(t.Context(), "sandbox1"); err == nil {
		t.Fatal("start returned no error")
	}

	got := l.repo.sb
	want := sandbox.SupervisorFailedReason + ": " + why
	if got.State != models.StateStopped || got.StoppedReason != want {
		t.Errorf("the record says %s with the reason %q, want stopped with %q", got.State, got.StoppedReason, want)
	}
	if got.ExitStatus == nil || *got.ExitStatus != (models.ExitStatus{Code: models.SupervisorFailedExitCode}) {
		t.Errorf("the record holds the exit %+v, want the supervisor's %d", got.ExitStatus, models.SupervisorFailedExitCode)
	}
}

func TestStartNamesTheSandboxWhenTheRecordWriteFails(t *testing.T) {
	svc, _ := newService(t, &recorder{fail: []string{"repo.Update"}}, stopped())

	_, err := svc.Start(t.Context(), "sandbox1")
	if err == nil || !strings.Contains(err.Error(), "is running but its record was not updated") {
		t.Errorf("start returned %v, want the sandbox named as running", err)
	}
}

func TestStartOfAMissingSandboxIsNotFound(t *testing.T) {
	svc, l := newService(t, &recorder{}, stopped())
	l.repo.missing = true

	if _, err := svc.Start(t.Context(), "sandbox1"); !errors.Is(err, sandboxstate.ErrNotFound) {
		t.Errorf("start = %v, want not found", err)
	}
}

// Two verbs on one sandbox run one after the other: a stop that arrives during a start waits for it.
func TestTheVerbsOnOneSandboxAreSerialized(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, stopped())

	gate := make(chan struct{})
	l.provider.gate = gate
	l.provider.entered = make(chan struct{})
	entered := l.provider.entered

	started := make(chan error, 1)
	go func() {
		_, err := svc.Start(t.Context(), "sandbox1")
		started <- err
	}()

	<-entered

	stopped := make(chan error, 1)
	go func() {
		_, err := svc.Stop(t.Context(), "sandbox1")
		stopped <- err
	}()

	// The stop has no way to signal that it is waiting, so give it a moment to reach the lock.
	time.Sleep(20 * time.Millisecond)
	if slices.Contains(r.calls, "provider.Stop") {
		t.Fatal("the stop ran while the start held the sandbox")
	}

	close(gate)

	if err := <-started; err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := <-stopped; err != nil {
		t.Fatalf("stop: %v", err)
	}

	if !l.provider.stopped || l.repo.sb.State != models.StateStopped {
		t.Errorf("the stop that waited did not end the sandbox: %v", r.calls)
	}
}

// A verb that waits on a sandbox another verb holds gives up when its own deadline ends (SHARD-370).
func TestAVerbStopsWaitingForTheSandboxWhenItsContextEnds(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, stopped())

	gate := make(chan struct{})
	l.provider.gate = gate
	l.provider.entered = make(chan struct{})
	entered := l.provider.entered

	started := make(chan error, 1)
	go func() {
		_, err := svc.Start(t.Context(), "sandbox1")
		started <- err
	}()
	<-entered

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := svc.Stop(ctx, "sandbox1")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "busy with another verb") {
		t.Errorf("stop = %v, want the busy sandbox and the deadline", err)
	}

	close(gate)
	if err := <-started; err != nil {
		t.Fatalf("start: %v", err)
	}
	if n := svc.Locks(); n != 0 {
		t.Errorf("%d sandbox locks outlived the start and the stop that gave up", n)
	}
}

// pullingCreate starts a create whose pull never ends on its own, and returns once the pull is reached.
func pullingCreate(t *testing.T) (*sandbox.Service, layers, *recorder, <-chan error) {
	t.Helper()

	r := &recorder{}
	entered := make(chan struct{})
	svc, l := newService(t, r, models.Sandbox{}, func(c *sandbox.Config) { c.Images = stalledImages{r: r, entered: entered} })
	// The create never reached the substrate, so the substrate holds nothing for it.
	l.provider.status = models.Status{}

	created := make(chan error, 1)
	go func() {
		_, err := svc.Create(t.Context(), alpine())
		created <- err
	}()
	<-entered

	return svc, l, r, created
}

// An rm of a sandbox still pulling its image ends the pull, rather than wait for the registry (SHARD-370).
func TestRemoveEndsTheCreateStillPulling(t *testing.T) {
	svc, l, r, created := pullingCreate(t)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := svc.Remove(ctx, "sandbox1", false); err != nil {
		t.Fatalf("rm: %v", err)
	}

	if err := <-created; err == nil || !strings.Contains(err.Error(), "cancelled by shard remove") {
		t.Errorf("create = %v, want it cancelled by shard remove", err)
	}
	if !l.repo.deleted {
		t.Error("rm left the record of the create it ended")
	}
	for _, step := range []string{"provider.Create", "provider.Start"} {
		if slices.Contains(r.calls, step) {
			t.Errorf("%s ran after rm ended the create: %v", step, r.calls)
		}
	}
	if n := svc.Locks(); n != 0 {
		t.Errorf("%d sandbox locks outlived the create and the rm", n)
	}
}

// A stop of a sandbox still pulling ends the create, and the failed record it leaves is one only rm takes.
func TestStopEndsTheCreateStillPulling(t *testing.T) {
	svc, l, _, created := pullingCreate(t)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := svc.Stop(ctx, "sandbox1")
	var state *sandbox.StateError
	if !errors.As(err, &state) || state.Code != models.CodeSandboxFailed || !strings.Contains(err.Error(), "cancelled by shard stop") {
		t.Errorf("stop = %v, want the failed sandbox and the stop that cancelled it", err)
	}

	if err := <-created; err == nil || !strings.Contains(err.Error(), "cancelled by shard stop") {
		t.Errorf("create = %v, want it cancelled by shard stop", err)
	}
	if l.repo.sb.State != models.StateFailed {
		t.Errorf("the record says %q, want failed", l.repo.sb.State)
	}
}

// A create whose record an rm freed before it took the sandbox builds nothing for the id.
func TestCompleteOfARemovedSandboxBuildsNothing(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, models.Sandbox{})

	sb, err := svc.Prepare(t.Context(), alpine())
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	l.repo.missing = true

	if err := svc.Complete(t.Context(), sb.ID, alpine()); err != nil {
		t.Fatalf("complete: %v", err)
	}
	for _, step := range []string{"images.Pull", "provider.Create", "repo.Update"} {
		if slices.Contains(r.calls, step) {
			t.Errorf("%s ran for a sandbox rm had freed: %v", step, r.calls)
		}
	}
}

// A stop that lands before the create takes the sandbox fails the record, and the create then builds nothing.
func TestStopBeforeTheCreateTakesTheSandboxBuildsNothing(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, models.Sandbox{})

	sb, err := svc.Prepare(t.Context(), alpine())
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}

	_, err = svc.Stop(t.Context(), sb.ID)
	var state *sandbox.StateError
	if !errors.As(err, &state) || state.Code != models.CodeSandboxFailed || !strings.Contains(err.Error(), "cancelled by shard stop") {
		t.Errorf("stop = %v, want the failed sandbox and the stop that cancelled it", err)
	}

	if err := svc.Complete(t.Context(), sb.ID, alpine()); err != nil {
		t.Fatalf("complete: %v", err)
	}
	for _, step := range []string{"images.Pull", "provider.Create", "provider.Start", "provider.Stop"} {
		if slices.Contains(r.calls, step) {
			t.Errorf("%s ran for a create the stop had ended: %v", step, r.calls)
		}
	}
	if l.repo.sb.State != models.StateFailed || !strings.Contains(l.repo.sb.FailedReason, "cancelled by shard stop") {
		t.Errorf("the record says %q (%q), want failed by the stop", l.repo.sb.State, l.repo.sb.FailedReason)
	}
}

// A stop that lands while the create waits for the sandbox fails it, even when the image is already cached.
func TestStopWhileTheCreateWaitsForTheSandboxBuildsNothing(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, models.Sandbox{})
	l.provider.status = models.Status{}

	sb, err := svc.Prepare(t.Context(), alpine())
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	unlock, err := svc.Hold(t.Context(), sb.ID)
	if err != nil {
		t.Fatalf("hold: %v", err)
	}

	created := make(chan error, 1)
	go func() { created <- svc.Complete(t.Context(), sb.ID, alpine()) }()
	waitForWaiters(t, svc, sb.ID, 2)

	stopped := make(chan error, 1)
	go func() {
		_, err := svc.Stop(t.Context(), sb.ID)
		stopped <- err
	}()
	waitForWaiters(t, svc, sb.ID, 3)
	unlock()

	err = <-stopped
	var state *sandbox.StateError
	if !errors.As(err, &state) || state.Code != models.CodeSandboxFailed || !strings.Contains(err.Error(), "cancelled by shard stop") {
		t.Errorf("stop = %v, want the failed sandbox and the stop that cancelled it", err)
	}
	if err := <-created; err != nil && !strings.Contains(err.Error(), "cancelled by shard stop") {
		t.Errorf("create = %v, want nothing built or it cancelled by shard stop", err)
	}
	for _, step := range []string{"provider.Create", "provider.Start"} {
		if slices.Contains(r.calls, step) {
			t.Errorf("%s ran for a create the stop had ended: %v", step, r.calls)
		}
	}
	if l.repo.sb.State != models.StateFailed {
		t.Errorf("the record says %q, want failed", l.repo.sb.State)
	}
}

// waitForWaiters polls until want verbs hold or wait on the sandbox's lock, and fails the test past the deadline.
func waitForWaiters(t *testing.T, svc *sandbox.Service, id string, want int) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for svc.Waiters(id) != want {
		if time.Now().After(deadline) {
			t.Fatalf("%d verbs on the lock of %s, want %d", svc.Waiters(id), id, want)
		}

		time.Sleep(time.Millisecond)
	}
}

// A stop ends the processes and keeps the record, the lease, the address and the writable layer.
func TestStopKeepsWhatOnlyRmFrees(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())

	sb, err := svc.Stop(t.Context(), "sandbox1")
	if err != nil {
		t.Fatalf("stop: %v", err)
	}

	if !l.provider.stopped {
		t.Errorf("stop never reached the provider: %v", r.calls)
	}
	if l.provider.removed {
		t.Error("stop removed the sandbox from the substrate, which frees the writable layer")
	}
	if l.net.released {
		t.Error("stop released the address, which a start would then not get back")
	}
	if l.repo.deleted {
		t.Error("stop deleted the record, which is the only handle a start has")
	}
	if sb.ID != "sandbox1" || sb.State != models.StateStopped {
		t.Errorf("stop answered %+v, want sandbox1 stopped", sb)
	}
}

func TestStopRecordsTheExitStatus(t *testing.T) {
	svc, l := newService(t, &recorder{}, running())
	l.provider.exit = models.ExitStatus{Code: 143, Signal: 15}

	if _, err := svc.Stop(t.Context(), "sandbox1"); err != nil {
		t.Fatalf("stop: %v", err)
	}

	sb := l.repo.sb
	if sb.State != models.StateStopped {
		t.Errorf("the record says %q, want stopped", sb.State)
	}
	if sb.PID != 0 {
		t.Errorf("the record still names the host pid %d of a sandbox that is gone", sb.PID)
	}
	if sb.ExitStatus == nil || sb.ExitStatus.Signal != 15 {
		t.Errorf("the record holds the exit status %+v, want the SIGTERM the stop sent", sb.ExitStatus)
	}
}

// A shard-init that dies on the way down outranks the entrypoint exit the record took: its 125 and its reason are what inspect shows (SHARD-290).
func TestStopRecordsTheExitAndTheReasonOfAShardInitThatDied(t *testing.T) {
	sb := running()
	sb.ExitStatus = &models.ExitStatus{Code: 3}
	svc, l := newService(t, &recorder{}, sb)
	why := "supervisor: forward the stop to the entrypoint: operation not permitted"
	l.provider.failsOnStop = why

	if _, err := svc.Stop(t.Context(), "sandbox1"); err != nil {
		t.Fatalf("stop: %v", err)
	}

	got := l.repo.sb
	want := sandbox.SupervisorFailedReason + ": " + why
	if got.State != models.StateStopped || got.StoppedReason != want {
		t.Errorf("the record says %s with the reason %q, want stopped with %q", got.State, got.StoppedReason, want)
	}
	if got.ExitStatus == nil || *got.ExitStatus != (models.ExitStatus{Code: models.SupervisorFailedExitCode}) {
		t.Errorf("the record holds the exit %+v, want the supervisor's %d", got.ExitStatus, models.SupervisorFailedExitCode)
	}
}

// A stop that had to kill leaves no exit status: the supervisor died before it could record one.
func TestStopRecordsNoExitStatusWhenTheSandboxWasKilled(t *testing.T) {
	svc, l := newService(t, &recorder{}, running())
	l.provider.waitErr = models.ErrNoExitStatus

	if _, err := svc.Stop(t.Context(), "sandbox1"); err != nil {
		t.Fatalf("stop: %v", err)
	}

	sb := l.repo.sb
	if sb.State != models.StateStopped {
		t.Errorf("the record says %q, want stopped", sb.State)
	}
	if sb.ExitStatus != nil {
		t.Errorf("the record holds the exit status %+v of an entrypoint that never reported one", sb.ExitStatus)
	}
}

// A second stop must not overwrite the exit status the first one recorded.
func TestStopIsIdempotent(t *testing.T) {
	sb := running()
	sb.State = models.StateStopped
	sb.ExitStatus = &models.ExitStatus{Code: 143, Signal: 15}

	r := &recorder{}
	svc, l := newService(t, r, sb)

	if _, err := svc.Stop(t.Context(), "sandbox1"); err != nil {
		t.Fatalf("the second stop: %v", err)
	}

	if slices.Contains(r.calls, "provider.Stop") || slices.Contains(r.calls, "repo.Update") {
		t.Errorf("the second stop changed something: %v", r.calls)
	}
	if got := l.repo.sb.ExitStatus; got == nil || got.Signal != 15 {
		t.Errorf("the record holds %+v, want the exit status the first stop recorded", got)
	}
}

func TestStopRefusesAnIDThatNeverExisted(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	l.repo.missing = true

	_, err := svc.Stop(t.Context(), "sandbox1")
	if !errors.Is(err, sandboxstate.ErrNotFound) || !strings.Contains(err.Error(), "sandbox1") {
		t.Errorf("stop failed with %v, want not found naming the id", err)
	}
	if slices.Contains(r.calls, "provider.Stop") {
		t.Errorf("stop reached the substrate for an id that never existed: %v", r.calls)
	}
}

// The grace is fixed, so every stop hands the provider the one constant and nothing a caller chose (SHARD-460).
func TestStopGivesTheProviderTheFixedGrace(t *testing.T) {
	svc, l := newService(t, &recorder{}, running())

	if _, err := svc.Stop(t.Context(), "sandbox1"); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if got := l.provider.grace; got != models.StopGrace {
		t.Errorf("the provider got the grace %s, want the fixed %s", got, models.StopGrace)
	}
}

// A record write that fails must not report success: the sandbox is gone and nothing says so.
func TestStopReportsARecordWriteThatFailed(t *testing.T) {
	svc, _ := newService(t, &recorder{fail: []string{"repo.Update"}}, running())

	if _, err := svc.Stop(t.Context(), "sandbox1"); err == nil {
		t.Fatal("a forced failure returned no error")
	}
}

func TestStopReportsAWaitThatFailedForAnotherReason(t *testing.T) {
	svc, l := newService(t, &recorder{}, running())
	l.provider.waitErr = errors.New("the exit status was unreadable")

	if _, err := svc.Stop(t.Context(), "sandbox1"); err == nil {
		t.Fatal("an unreadable exit status returned no error")
	}
}

// runsc can report a sandbox alive for a moment after a clean stop, and a rm that lands there refuses it.
func TestStopWaitsForTheSubstrateToReportTheSandboxGone(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	l.provider.aliveAfterStop = 3

	sb, err := svc.Stop(t.Context(), "sandbox1")
	if err != nil {
		t.Fatalf("stop: %v", err)
	}

	if l.provider.aliveAfterStop != 0 {
		t.Errorf("stop returned with %d alive answers left", l.provider.aliveAfterStop)
	}
	if sb.State != models.StateStopped {
		t.Errorf("the record says %s, want stopped", sb.State)
	}
	if err := svc.Remove(t.Context(), "sandbox1", false); err != nil {
		t.Fatalf("the rm right after the stop was refused: %v", err)
	}
}

// Refuse, never downgrade: a sandbox the substrate still reports alive is not written down as stopped.
func TestStopRefusesASandboxThatNeverSettles(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running(), func(cfg *sandbox.Config) { cfg.StopSettle = 20 * time.Millisecond })
	l.provider.aliveAfterStop = -1

	_, err := svc.Stop(t.Context(), "sandbox1")
	if err == nil {
		t.Fatal("a sandbox that never settles returned no error")
	}
	if !strings.Contains(err.Error(), "sandbox1") || !strings.Contains(err.Error(), string(models.StateRunning)) {
		t.Errorf("the error is %q, want it to name the sandbox and the state the substrate reports", err)
	}
	if l.repo.sb.State != models.StateRunning {
		t.Errorf("the record says %s, want it left as it was", l.repo.sb.State)
	}
}

func TestStopEndsTheWaitWhenTheContextIsCancelled(t *testing.T) {
	svc, l := newService(t, &recorder{}, running())
	l.provider.aliveAfterStop = -1

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := svc.Stop(ctx, "sandbox1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop answered %v, want the context's own error", err)
	}
}

// A start that failed after the substrate came up leaves a stopped record over a live sandbox.
func TestStopEndsASandboxWhoseRecordSaysStopped(t *testing.T) {
	sb := running()
	sb.State = models.StateStopped

	r := &recorder{}
	svc, l := newService(t, r, sb)
	l.provider.status = models.Status{Exists: true, State: models.StateRunning, PID: 9}

	if _, err := svc.Stop(t.Context(), "sandbox1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !l.provider.stopped {
		t.Errorf("the live sandbox was not stopped: %v", r.calls)
	}
}

func stoppedOnTheHost(t *testing.T, r *recorder) (*sandbox.Service, layers) {
	t.Helper()

	sb := running()
	sb.State = models.StateStopped

	return newService(t, r, sb)
}

// The record comes after the mount and the namespace, and the rules after the record they render from.
func TestRemoveFreesEveryHoldingInOrder(t *testing.T) {
	r := &recorder{}
	svc, _ := stoppedOnTheHost(t, r)

	if err := svc.Remove(t.Context(), "sandbox1", false); err != nil {
		t.Fatalf("rm: %v", err)
	}

	want := []string{"provider.Remove", "net.Release", "repo.Delete", "net.ReapplyAll", "repo.List", "substrate.ReleaseRoot"}
	if got := r.calls[len(r.calls)-len(want):]; !slices.Equal(got, want) {
		t.Errorf("rm freed %v, want %v", got, want)
	}
}

// runsc bind mounts a null-netns into its own root and never drops it, so the last rm does.
func TestRemoveOfTheLastSandboxDropsWhatTheSubstrateKeeps(t *testing.T) {
	r := &recorder{}
	svc, l := stoppedOnTheHost(t, r)

	if err := svc.Remove(t.Context(), "sandbox1", false); err != nil {
		t.Fatalf("rm: %v", err)
	}
	if !l.substrate.dropped {
		t.Errorf("the rm of the last sandbox left the substrate mount: %v", r.calls)
	}
}

func TestRemoveKeepsWhatTheSubstrateSharesWhileASandboxIsLeft(t *testing.T) {
	svc, l := stoppedOnTheHost(t, &recorder{})
	l.repo.left = []models.Sandbox{{ID: "sandbox2"}}

	if err := svc.Remove(t.Context(), "sandbox1", false); err != nil {
		t.Fatalf("rm: %v", err)
	}
	if l.substrate.dropped {
		t.Error("the rm dropped the substrate mount while another sandbox still uses the root")
	}
}

// SHARD-343: a record that will not read may name this substrate, so the last rm keeps the root rather than releasing it.
func TestRemoveKeepsTheSubstrateWhileARecordIsUnreadable(t *testing.T) {
	svc, l := stoppedOnTheHost(t, &recorder{})
	l.repo.listErr = &sandboxstate.UnreadableError{ID: "broken", Err: errors.New("decode sandbox.json: unexpected end of JSON input")}

	if err := svc.Remove(t.Context(), "sandbox1", false); err != nil {
		t.Fatalf("rm: %v", err)
	}
	if l.substrate.dropped {
		t.Error("the rm dropped the substrate mount while a record could not be read")
	}
}

func TestRemoveRefusesARunningSandbox(t *testing.T) {
	r := &recorder{}
	svc, _ := newService(t, r, running())

	err := svc.Remove(t.Context(), "sandbox1", false)

	var refused *sandbox.StateError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "shard stop sandbox1") {
		t.Fatalf("rm failed with %v, want a state error that says to stop the sandbox first", err)
	}

	for _, step := range []string{"provider.Remove", "net.Release", "repo.Delete"} {
		if slices.Contains(r.calls, step) {
			t.Errorf("the refused rm still freed %s: %v", step, r.calls)
		}
	}
}

// force is the shorthand for the stop the operator would type first.
func TestRemoveForceStopsThenRemoves(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())

	if err := svc.Remove(t.Context(), "sandbox1", true); err != nil {
		t.Fatalf("rm --force: %v", err)
	}

	if !l.provider.stopped || !l.provider.removed {
		t.Errorf("rm --force stopped=%v removed=%v, want both", l.provider.stopped, l.provider.removed)
	}
	if l.provider.grace != models.StopGrace {
		t.Errorf("the stop under rm --force got the grace %s, want the fixed %s a stop gives", l.provider.grace, models.StopGrace)
	}

	stopped := slices.Index(r.calls, "provider.Stop")
	removed := slices.Index(r.calls, "provider.Remove")
	if stopped < 0 || removed < stopped {
		t.Errorf("rm --force ran %v, want the stop before the remove", r.calls)
	}
}

// A pause ends the process, so the substrate answers gone (gVisor) or stopped (Firecracker, vz); only the record says paused (SHARD-281).
func TestRemoveRefusesAPausedSandbox(t *testing.T) {
	for name, status := range map[string]models.Status{
		"the container is gone": {},
		"the vmm is stopped":    {Exists: true, State: models.StateStopped},
	} {
		t.Run(name, func(t *testing.T) {
			r := &recorder{}
			svc, l := newService(t, r, pausedSandbox())
			l.provider.status = status

			err := svc.Remove(t.Context(), "sandbox1", false)

			var refused *sandbox.StateError
			if !errors.As(err, &refused) || refused.Code != models.CodeSandboxNotStopped || err.Error() != "sandbox sandbox1 is paused: stop it first with shard stop sandbox1, or pass --force" {
				t.Fatalf("rm failed with %v, want a sandbox_not_stopped error that names the pause and the stop", err)
			}

			for _, step := range []string{"provider.Stop", "provider.Remove", "net.Release", "repo.Delete"} {
				if slices.Contains(r.calls, step) {
					t.Errorf("the refused rm still ran %s: %v", step, r.calls)
				}
			}
		})
	}
}

// The record alone decides a paused rm, so a probe that fails or wedges never turns the 409 into a 500 or a 504.
func TestRemoveOfAPausedSandboxNeverProbesTheSubstrate(t *testing.T) {
	r := &recorder{fail: []string{"provider.Status"}}
	svc, _ := newService(t, r, pausedSandbox())

	err := svc.Remove(t.Context(), "sandbox1", false)

	var refused *sandbox.StateError
	if !errors.As(err, &refused) || refused.Code != models.CodeSandboxNotStopped {
		t.Fatalf("rm with a failing status probe returned %v, want the sandbox_not_stopped refusal", err)
	}

	if slices.Contains(r.calls, "provider.Status") {
		t.Errorf("the refused rm of a paused sandbox probed the substrate: %v", r.calls)
	}

	wedged := &recorder{}
	svc, l := newService(t, wedged, pausedSandbox())
	l.provider.statusGate = make(chan struct{})
	l.provider.stopUnwedges = true

	if err := svc.Remove(t.Context(), "sandbox1", true); err != nil {
		t.Fatalf("rm --force with a wedged status probe: %v", err)
	}
	stopped := slices.Index(wedged.calls, "provider.Stop")
	probed := slices.Index(wedged.calls, "provider.Status")
	if l.provider.reclaimed || stopped < 0 || probed < stopped {
		t.Errorf("rm --force of a paused sandbox ran %v and reclaimed %t, want the stop before any probe and no kill", wedged.calls, l.provider.reclaimed)
	}
}

func TestRemoveForceStopsAPausedSandboxThenRemoves(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, pausedSandbox())
	l.provider.status = models.Status{}

	if err := svc.Remove(t.Context(), "sandbox1", true); err != nil {
		t.Fatalf("rm --force: %v", err)
	}

	stopped := slices.Index(r.calls, "provider.Stop")
	removed := slices.Index(r.calls, "provider.Remove")
	if stopped < 0 || removed < stopped {
		t.Errorf("rm --force of a paused sandbox ran %v, want the stop before the remove", r.calls)
	}
}

// The record dies last, so an id with no record has nothing else left either.
func TestRemoveOfAMissingSandboxIsNotFound(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	l.repo.missing = true

	if err := svc.Remove(t.Context(), "sandbox1", false); !errors.Is(err, sandboxstate.ErrNotFound) {
		t.Fatalf("rm of an id that is already gone = %v, want not found", err)
	}

	for _, step := range []string{"provider.Remove", "net.Release", "repo.Delete"} {
		if slices.Contains(r.calls, step) {
			t.Errorf("rm of an id that is already gone freed %s: %v", step, r.calls)
		}
	}
}

// A partial rm must be legible, so the error lists every holding from the one that failed onwards.
func TestRemoveNamesWhatIsLeftOnTheHost(t *testing.T) {
	svc, l := stoppedOnTheHost(t, &recorder{fail: []string{"net.Release"}})

	err := svc.Remove(t.Context(), "sandbox1", false)
	if err == nil {
		t.Fatal("a forced failure returned no error")
	}

	for _, left := range []string{"netns, veth and address lease", "record and state directory", "host rules"} {
		if !strings.Contains(err.Error(), left) {
			t.Errorf("rm failed with %v, want it to name the %s it left behind", err, left)
		}
	}
	if strings.Contains(err.Error(), "runtime state and rootfs mount") {
		t.Errorf("rm failed with %v, but it did free the runtime state", err)
	}
	if l.repo.deleted {
		t.Error("rm deleted the record past a failure, so nothing can reach what is left")
	}
}

// The rules are keyed by the address, so an rm that leaves them fronts whoever takes that address next.
func TestRemoveReappliesTheRulesAfterTheRecordIsGone(t *testing.T) {
	r := &recorder{}
	svc, _ := stoppedOnTheHost(t, r)

	if err := svc.Remove(t.Context(), "sandbox1", false); err != nil {
		t.Fatalf("rm: %v", err)
	}

	reapplied := slices.Index(r.calls, "net.ReapplyAll")
	if reapplied < 0 {
		t.Fatalf("the rm never reapplied the host rules: %v", r.calls)
	}
	if deleted := slices.Index(r.calls, "repo.Delete"); reapplied < deleted {
		t.Errorf("the rm reapplied the rules before it deleted the record: %v", r.calls)
	}
}

// A failed reapply leaves rules for a sandbox that is gone, so the error must say so.
func TestRemoveNamesTheRulesItLeft(t *testing.T) {
	svc, _ := stoppedOnTheHost(t, &recorder{fail: []string{"net.ReapplyAll"}})

	err := svc.Remove(t.Context(), "sandbox1", false)
	if err == nil {
		t.Fatal("a forced failure returned no error")
	}
	if !strings.Contains(err.Error(), "host rules") {
		t.Errorf("rm failed with %v, want it to name the host rules it left behind", err)
	}
}
