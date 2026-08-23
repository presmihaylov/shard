//go:build integration

package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/netns"
)

// stubbornEntrypoint ignores SIGTERM, so only the kill after the grace ends it.
var stubbornEntrypoint = []string{"/bin/sh", "-c", "trap '' TERM; while true; do sleep 1; done"}

// TestStopKeepsTheAddressAndTheRecordOnTheHost is the boundary between the two verbs. The processes end and the
// record, the lease, the address, the namespace and the link all stay, so a start can follow.
func TestStopKeepsTheAddressAndTheRecordOnTheHost(t *testing.T) {
	app, out := newCreateApp(t)
	deps := createApp(t, app)

	id := create(t, app, out, "/bin/sleep", "600")
	t.Cleanup(func() { cleanUp(t, deps, id) })

	before, err := records(t, app.Root).Get(id)
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}

	if err := app.Run(t.Context(), []string{"stop", id}); err != nil {
		t.Fatalf("stop: %v", err)
	}

	status, err := deps.provider.Status(t.Context(), id)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Alive() {
		t.Errorf("the substrate says %+v after a stop, want a sandbox that is no longer alive", status)
	}

	after, err := records(t, app.Root).Get(id)
	if err != nil {
		t.Fatalf("the stop took the record with it: %v", err)
	}
	if after.State != models.StateStopped {
		t.Errorf("the record says %q, want stopped", after.State)
	}
	if after.Address != before.Address {
		t.Errorf("the record holds the address %s, want the %s a start would keep", after.Address, before.Address)
	}

	// The entrypoint answered the SIGTERM the supervisor forwarded, so the supervisor recorded it.
	if after.ExitStatus == nil || after.ExitStatus.Signal != 15 {
		t.Errorf("the record holds the exit status %+v, want the SIGTERM the stop sent", after.ExitStatus)
	}

	if held := leases(t, app.Root); len(held) != 1 {
		t.Errorf("the stop left the leases %v, want the one the sandbox keeps", held)
	}
	if exists, err := netns.NamespaceExists(id); err != nil || !exists {
		t.Errorf("the stop took the namespace of %s with it: %v", id, err)
	}
	if !hasLink(t, before.HostInterface) {
		t.Errorf("the stop took the host interface %s with it", before.HostInterface)
	}
}

// TestStopKillsAnEntrypointThatIgnoresTheSignal is the other half of the grace: an entrypoint that
// never answers is killed, and nothing then records how it ended.
func TestStopKillsAnEntrypointThatIgnoresTheSignal(t *testing.T) {
	app, out := newCreateApp(t)
	deps := createApp(t, app)

	id := create(t, app, out, stubbornEntrypoint...)
	t.Cleanup(func() { cleanUp(t, deps, id) })

	grace := 2 * time.Second
	started := time.Now()
	if err := app.Run(t.Context(), []string{"stop", "--time", grace.String(), id}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	took := time.Since(started)

	if took < grace {
		t.Errorf("the stop took %s, want it to wait out the %s grace before it killed", took, grace)
	}

	status, err := deps.provider.Status(t.Context(), id)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Alive() {
		t.Errorf("the substrate says %+v, want a sandbox the kill ended", status)
	}

	sb, err := records(t, app.Root).Get(id)
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}
	if sb.State != models.StateStopped {
		t.Errorf("the record says %q, want stopped", sb.State)
	}

	// The kill takes the supervisor too, so nothing was left to write how the entrypoint ended.
	if sb.ExitStatus != nil {
		t.Errorf("the record holds the exit status %+v of an entrypoint that was killed", sb.ExitStatus)
	}
}

// TestASecondStopChangesNothingOnTheHost: the second stop must change nothing, and the exit status the first one
// recorded is the one that happened.
func TestASecondStopChangesNothingOnTheHost(t *testing.T) {
	app, out := newCreateApp(t)
	deps := createApp(t, app)

	id := create(t, app, out, "/bin/sleep", "600")
	t.Cleanup(func() { cleanUp(t, deps, id) })

	if err := app.Run(t.Context(), []string{"stop", id}); err != nil {
		t.Fatalf("the first stop: %v", err)
	}

	first, err := records(t, app.Root).Get(id)
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}

	if err := app.Run(t.Context(), []string{"stop", id}); err != nil {
		t.Fatalf("the second stop: %v", err)
	}

	second, err := records(t, app.Root).Get(id)
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}

	if second.State != first.State || *second.ExitStatus != *first.ExitStatus {
		t.Errorf("the second stop left %+v, want the %+v the first one wrote", second.ExitStatus, first.ExitStatus)
	}
}

func TestStopRefusesAnIDTheHostNeverHeld(t *testing.T) {
	app, _ := newCreateApp(t)

	err := app.Run(t.Context(), []string{"stop", "no-such-sandbox"})
	if err == nil {
		t.Fatal("the stop of an id that never existed returned no error")
	}
	if !strings.Contains(err.Error(), "no-such-sandbox") {
		t.Errorf("the stop failed with %v, want it to name the id", err)
	}
}

func TestRmRefusesASandboxThatIsStillUp(t *testing.T) {
	app, out := newCreateApp(t)
	deps := createApp(t, app)

	id := create(t, app, out, "/bin/sleep", "600")
	t.Cleanup(func() { cleanUp(t, deps, id) })

	err := app.Run(t.Context(), []string{"rm", id})
	if err == nil {
		t.Fatal("the rm of a running sandbox returned no error")
	}
	if !strings.Contains(err.Error(), "shard stop "+id) {
		t.Errorf("the rm failed with %v, want it to say to stop the sandbox first", err)
	}

	if _, err := records(t, app.Root).Get(id); err != nil {
		t.Errorf("the refused rm took the record with it: %v", err)
	}

	status, err := deps.provider.Status(t.Context(), id)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Alive() {
		t.Errorf("the refused rm left %+v, want the sandbox it refused to touch", status)
	}
}

// TestRmFreesEveryHolding is the SHARD-24 acceptance criterion: nothing is left on the host.
func TestRmFreesEveryHolding(t *testing.T) {
	app, out := newCreateApp(t)
	deps := createApp(t, app)

	id := create(t, app, out, "/bin/sleep", "600")
	// A step below that fails ends the test with the sandbox still up, and the devbox is shared.
	t.Cleanup(func() { cleanUp(t, deps, id) })

	sb, err := records(t, app.Root).Get(id)
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}

	if err := app.Run(t.Context(), []string{"stop", id}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := app.Run(t.Context(), []string{"rm", id}); err != nil {
		t.Fatalf("rm: %v", err)
	}

	assertNothingLeft(t, app.Root, sb)
}

// TestRmForceEndsASandboxThatIsStillUp: --force is the shorthand for the stop the operator would type first.
func TestRmForceEndsASandboxThatIsStillUp(t *testing.T) {
	app, out := newCreateApp(t)
	deps := createApp(t, app)

	id := create(t, app, out, "/bin/sleep", "600")
	t.Cleanup(func() { cleanUp(t, deps, id) })

	sb, err := records(t, app.Root).Get(id)
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}

	if err := app.Run(t.Context(), []string{"rm", "--force", id}); err != nil {
		t.Fatalf("rm --force: %v", err)
	}

	assertNothingLeft(t, app.Root, sb)
}

// TestASecondRmFindsNothingToFree: the record dies last, so an id with no record has nothing else left either.
func TestASecondRmFindsNothingToFree(t *testing.T) {
	app, out := newCreateApp(t)
	deps := createApp(t, app)

	id := create(t, app, out, "/bin/sleep", "600")
	t.Cleanup(func() { cleanUp(t, deps, id) })

	if err := app.Run(t.Context(), []string{"rm", "--force", id}); err != nil {
		t.Fatalf("the first rm: %v", err)
	}
	if err := app.Run(t.Context(), []string{"rm", id}); err != nil {
		t.Fatalf("the second rm: %v", err)
	}
}

// create runs the command under test and answers with the id it printed.
func create(t *testing.T, app App, out *bytes.Buffer, argv ...string) string {
	t.Helper()

	args := append([]string{"create", testImage, "--"}, argv...)
	if err := app.Run(t.Context(), args); err != nil {
		t.Fatalf("create: %v", err)
	}

	id := onlySandbox(t, app.Root)
	out.Reset()

	return id
}

// assertNothingLeft checks every holding a sandbox has, which is what the ticket asks a stranger to verify.
func assertNothingLeft(t *testing.T, root string, sb models.Sandbox) {
	t.Helper()

	left, err := records(t, root).List()
	if err != nil {
		t.Fatalf("list the records: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("the rm left the records %+v", left)
	}

	if held := leases(t, root); len(held) != 0 {
		t.Errorf("the rm left the leases %v", held)
	}
	if held := containers(t, root); len(held) != 0 {
		t.Errorf("the rm left the runsc containers %v", held)
	}

	if exists, err := netns.NamespaceExists(sb.ID); err != nil || exists {
		t.Errorf("the rm left the namespace of %s: %v", sb.ID, err)
	}
	if hasLink(t, sb.HostInterface) {
		t.Errorf("the rm left the host interface %s", sb.HostInterface)
	}

	// The whole root, not only the sandbox tree: the rm that empties the root also gives back the
	// null-netns runsc bind mounts into its own root and never drops.
	mounts, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		t.Fatalf("read the mount table: %v", err)
	}
	if strings.Contains(string(mounts), root) {
		t.Errorf("the rm left a mount under %s", root)
	}
}
