package vzvm

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/supervisor"
)

// A replayed state lands the exit and the restart count, and a later replay with a newer exit replaces what a read sees.
func TestARepliedStateLandsTheLastExitAndTheRestarts(t *testing.T) {
	p := &Provider{}
	m := &machine{dir: t.TempDir()}
	first := supervisor.Message{Kind: supervisor.KindState, Ready: true, Exit: &models.ExitStatus{Code: 3}, Restarts: &models.RestartCount{Count: 1}}
	second := supervisor.Message{Kind: supervisor.KindState, Ready: true, Exit: &models.ExitStatus{Code: 5}, Restarts: &models.RestartCount{Count: 2}}

	if err := p.record(m, first); err != nil {
		t.Fatal(err)
	}
	if !m.started {
		t.Error("the state did not mark the machine started")
	}
	exit, found, err := bundle.ReadExitStatus(filepath.Join(m.dir, exitFile))
	if err != nil || !found || exit.Code != 3 {
		t.Fatalf("exit = %+v, %v, %v; want code 3", exit, found, err)
	}
	// The guest died again while no daemon held the shim, so the next adoption must show that exit, not the first.
	if err := p.record(m, second); err != nil {
		t.Fatal(err)
	}
	exit, found, err = bundle.ReadExitStatus(filepath.Join(m.dir, exitFile))
	if err != nil || !found || exit.Code != 5 {
		t.Fatalf("exit after the second replay = %+v, %v, %v; want code 5", exit, found, err)
	}
	count, err := bundle.Bundle{RestartFile: filepath.Join(m.dir, restartsFile)}.RestartCount()
	if err != nil || count.Count != 2 {
		t.Fatalf("restarts = %+v, %v; want 2", count, err)
	}
}

// The guest writes its own resolver files, so the record carries what the network leased and the name the sandbox answers to.
func TestARecordCarriesTheResolverFilesTheGuestWrites(t *testing.T) {
	spec := models.SandboxSpec{
		ID: "sb-1", Name: "web", RootFS: t.TempDir(), Entrypoint: []string{"/bin/true"},
		Network: models.NetworkSpec{
			Address:     netip.MustParsePrefix("10.87.0.2/16"),
			Gateway:     netip.MustParseAddr("10.87.0.1"),
			Nameservers: []netip.Addr{netip.MustParseAddr("10.87.0.1")},
		},
	}

	r, err := recordOf(spec)
	if err != nil {
		t.Fatal(err)
	}
	if r.Hostname != "web" || !reflect.DeepEqual(r.Nameservers, []string{"10.87.0.1"}) {
		t.Errorf("recorded hostname %q and nameservers %v, want web and the gateway", r.Hostname, r.Nameservers)
	}

	spec.Name = ""
	r, err = recordOf(spec)
	if err != nil {
		t.Fatal(err)
	}
	if r.Hostname != "sb-1" {
		t.Errorf("an unnamed sandbox recorded hostname %q, want its id", r.Hostname)
	}

	spec.Network = models.NetworkSpec{}
	r, err = recordOf(spec)
	if err != nil {
		t.Fatal(err)
	}
	if r.Hostname != "" || r.Nameservers != nil {
		t.Errorf("a sandbox without a network recorded %+v, want no resolver files", r)
	}
}

// A log file that refuses a write marks the sandbox lost and ends the follow: a redial would carry the same broken file.
func TestALogWriteThatFailsMarksTheSandboxLostInsteadOfRedialing(t *testing.T) {
	p := &Provider{}
	m := &machine{id: "sb-1"}
	guest, host := net.Pipe()
	defer guest.Close()

	done := make(chan struct{})
	go func() {
		p.followLogs(context.Background(), m, host, brokenLog{})
		close(done)
	}()
	if _, err := guest.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the follow went on after the log refused a write")
	}
	if m.lost == nil || !strings.Contains(m.lost.Error(), "the log stopped") || !errors.Is(m.lost, errNoSpace) {
		t.Fatalf("lost = %v, want the log write failure", m.lost)
	}
}

var errNoSpace = errors.New("no space left on device")

type brokenLog struct{}

func (brokenLog) Write([]byte) (int, error) { return 0, errNoSpace }

func (brokenLog) Close() error { return nil }

// An OOM message leaves a marker the status reads once the guest is gone, and the next boot must clear it.
func TestAnOOMMessageMarksTheMachineKilledUnderTheBound(t *testing.T) {
	p := &Provider{}
	m := &machine{id: "sb-1", dir: t.TempDir()}

	if err := p.record(m, supervisor.Message{Kind: supervisor.KindOOM}); err != nil {
		t.Fatal(err)
	}
	if m.status(p).OOMKilled {
		t.Fatal("the status blamed the bound while the guest still ran")
	}
	m.gone = true
	status := m.status(p)
	if status.State != models.StateStopped || !status.OOMKilled {
		t.Fatalf("status = %+v; want stopped and OOMKilled", status)
	}
	if !oomKilled(m.dir) {
		t.Fatal("no marker for a status read with no machine")
	}
}
