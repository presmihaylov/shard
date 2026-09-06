package sandbox_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/sandbox"
)

// attachable is granted with the two policies an attach test picks from already in the store.
func attachable(t *testing.T, r *recorder, sb models.Sandbox) (*sandbox.Service, layers, bundle.Bundle) {
	t.Helper()

	svc, l, b := granted(t, r, sb)
	for _, name := range []string{"locked", "open"} {
		if err := l.policies.Set(models.Policy{Name: name}); err != nil {
			t.Fatalf("policies.Set %s: %v", name, err)
		}
	}

	return svc, l, b
}

func TestAttachPolicyWritesTheRecordAndAppliesTheRules(t *testing.T) {
	r := &recorder{}
	svc, l, b := attachable(t, r, stopped())

	sb, err := svc.AttachPolicy(context.Background(), "web", "locked")
	if err != nil {
		t.Fatalf("AttachPolicy: %v", err)
	}

	if sb.Policy != "locked" {
		t.Errorf("the record holds %q, want the policy", sb.Policy)
	}
	if l.repo.sb.Policy != "locked" {
		t.Errorf("the stored record holds %q", l.repo.sb.Policy)
	}
	if !slices.Contains(r.calls, "net.Reapply") {
		t.Errorf("the attach did not reapply the host rules: %v", r.calls)
	}

	// The attach fronts a sandbox that held neither a policy nor a secret, so the guest must trust the proxy.
	trusted, held := guestEnv(t, b, "SSL_CERT_FILE")
	if !held {
		t.Fatal("the attach did not plant the proxy CA")
	}
	merged, err := os.ReadFile(filepath.Join(b.Upper, trusted))
	if err != nil {
		t.Fatalf("read the merged CA bundle: %v", err)
	}
	if !strings.HasPrefix(string(merged), imageRoots) || len(merged) == len(imageRoots) {
		t.Errorf("the merged bundle is not the image roots plus the proxy CA:\n%s", merged)
	}
}

func TestAttachPolicyReplacesTheOneItHolds(t *testing.T) {
	svc, l, _ := attachable(t, &recorder{}, stopped())

	if _, err := svc.AttachPolicy(context.Background(), "web", "locked"); err != nil {
		t.Fatalf("the first AttachPolicy: %v", err)
	}

	sb, err := svc.AttachPolicy(context.Background(), "web", "open")
	if err != nil {
		t.Fatalf("the second AttachPolicy: %v", err)
	}
	if sb.Policy != "open" {
		t.Errorf("the record holds %q, want the policy the second attach named", sb.Policy)
	}
	if l.repo.sb.Policy != "open" {
		t.Errorf("the stored record holds %q", l.repo.sb.Policy)
	}
}

func TestAttachPolicyTakesASecondCall(t *testing.T) {
	svc, l, _ := attachable(t, &recorder{}, stopped())

	if _, err := svc.AttachPolicy(context.Background(), "web", "locked"); err != nil {
		t.Fatalf("the first AttachPolicy: %v", err)
	}
	if _, err := svc.AttachPolicy(context.Background(), "web", "locked"); err != nil {
		t.Fatalf("the second AttachPolicy: %v", err)
	}
	if l.repo.sb.Policy != "locked" {
		t.Errorf("the stored record holds %q after two attaches", l.repo.sb.Policy)
	}
}

// A sandbox the grant already fronted must not have the proxy CA appended to its bundle twice.
func TestAttachPolicyLeavesTheCAOfASandboxThatWasFronted(t *testing.T) {
	svc, _, b := attachable(t, &recorder{}, stopped())

	if _, err := svc.GrantSecret(context.Background(), "web", "TOKEN"); err != nil {
		t.Fatalf("GrantSecret: %v", err)
	}
	trusted, _ := guestEnv(t, b, "SSL_CERT_FILE")
	before, err := os.ReadFile(filepath.Join(b.Upper, trusted))
	if err != nil {
		t.Fatalf("read the merged CA bundle: %v", err)
	}

	if _, err := svc.AttachPolicy(context.Background(), "web", "locked"); err != nil {
		t.Fatalf("AttachPolicy: %v", err)
	}

	after, err := os.ReadFile(filepath.Join(b.Upper, trusted))
	if err != nil {
		t.Fatalf("read the merged CA bundle again: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("the attach rewrote the CA bundle of a sandbox that was already fronted:\n%s", after)
	}
}

func TestAttachPolicyRefusesARunningSandbox(t *testing.T) {
	svc, _, _ := attachable(t, &recorder{}, running())

	_, err := svc.AttachPolicy(context.Background(), "sandbox1", "locked")
	if err == nil {
		t.Fatal("the attach took a running sandbox")
	}
	if !strings.Contains(err.Error(), "stop it first") {
		t.Errorf("the error does not name the fix: %v", err)
	}
}

func TestAttachPolicyRefusesAPausedSandbox(t *testing.T) {
	svc, _, _ := attachable(t, &recorder{}, pausedSandbox())

	if _, err := svc.AttachPolicy(context.Background(), "sandbox1", "locked"); err == nil {
		t.Fatal("the attach took a paused sandbox, whose snapshot already holds the rules")
	}
}

func TestAttachPolicyRefusesAPolicyThatDoesNotExistAndWritesNothing(t *testing.T) {
	r := &recorder{}
	svc, l, b := attachable(t, r, stopped())

	if _, err := svc.AttachPolicy(context.Background(), "web", "missing"); err == nil {
		t.Fatal("the attach took a policy the store does not hold")
	}

	if l.repo.sb.Policy != "" {
		t.Errorf("the record holds %q after a refused attach", l.repo.sb.Policy)
	}
	if slices.Contains(r.calls, "net.Reapply") {
		t.Errorf("the refused attach applied the host rules: %v", r.calls)
	}
	if _, err := os.Stat(filepath.Join(b.Upper, "etc/ssl/certs/ca-certificates.crt")); !os.IsNotExist(err) {
		t.Errorf("the refused attach wrote the writable layer: %v", err)
	}
}

func TestAttachPolicyRefusesAnEmptyName(t *testing.T) {
	svc, _, _ := attachable(t, &recorder{}, stopped())

	if _, err := svc.AttachPolicy(context.Background(), "web", ""); err == nil {
		t.Fatal("the attach took no policy name")
	}
}

// A record that names a policy the host does not enforce is the one outcome the verb must not leave.
func TestAttachPolicyPutsTheRecordBackWhenTheHostRefusesTheRules(t *testing.T) {
	r := &recorder{fail: []string{"net.Reapply"}}
	svc, l, _ := attachable(t, r, stopped())

	_, err := svc.AttachPolicy(context.Background(), "web", "locked")
	if err == nil {
		t.Fatal("the attach reported success while the host refused the rules")
	}
	if !strings.Contains(err.Error(), "as it was") {
		t.Errorf("the error does not say the record went back: %v", err)
	}
	if l.repo.sb.Policy != "" {
		t.Errorf("the stored record holds %q after a failed apply", l.repo.sb.Policy)
	}
}

func TestDetachPolicyTakesThePolicyBackAndLeavesTheSecrets(t *testing.T) {
	svc, l, b := attachable(t, &recorder{}, stopped())

	if _, err := svc.GrantSecret(context.Background(), "web", "TOKEN"); err != nil {
		t.Fatalf("GrantSecret: %v", err)
	}
	if _, err := svc.AttachPolicy(context.Background(), "web", "locked"); err != nil {
		t.Fatalf("AttachPolicy: %v", err)
	}

	sb, err := svc.DetachPolicy(context.Background(), "web")
	if err != nil {
		t.Fatalf("DetachPolicy: %v", err)
	}

	if sb.Policy != "" {
		t.Errorf("the record still holds %q", sb.Policy)
	}
	if l.repo.sb.Policy != "" {
		t.Errorf("the stored record still holds %q", l.repo.sb.Policy)
	}
	if !slices.Contains(sb.Secrets, "TOKEN") {
		t.Errorf("the detach took the grant away: %v", sb.Secrets)
	}
	if _, held := guestEnv(t, b, "SSL_CERT_FILE"); !held {
		t.Error("the detach took the proxy CA away")
	}

	// A second detach changes nothing, so an interrupted one is finished by a retry.
	if _, err := svc.DetachPolicy(context.Background(), "web"); err != nil {
		t.Fatalf("the second DetachPolicy: %v", err)
	}
}

func TestDetachPolicyRefusesARunningSandbox(t *testing.T) {
	svc, _, _ := attachable(t, &recorder{}, running())

	_, err := svc.DetachPolicy(context.Background(), "sandbox1")
	if err == nil {
		t.Fatal("the detach took a running sandbox")
	}
	if !strings.Contains(err.Error(), "stop it first") {
		t.Errorf("the error does not name the fix: %v", err)
	}
}

func TestDetachPolicyPutsTheRecordBackWhenTheHostRefusesTheRules(t *testing.T) {
	r := &recorder{}
	svc, l, _ := attachable(t, r, stopped())

	if _, err := svc.AttachPolicy(context.Background(), "web", "locked"); err != nil {
		t.Fatalf("AttachPolicy: %v", err)
	}

	r.fail = []string{"net.Reapply"}
	if _, err := svc.DetachPolicy(context.Background(), "web"); err == nil {
		t.Fatal("the detach reported success while the host refused the rules")
	}
	if l.repo.sb.Policy != "locked" {
		t.Errorf("the stored record holds %q after a failed apply", l.repo.sb.Policy)
	}
}

// policy rm and the POLICY column read the record, so an attach is enough to make them follow.
func TestPolicyHoldersFollowsAnAttach(t *testing.T) {
	svc, l, _ := attachable(t, &recorder{}, stopped())

	if _, err := svc.AttachPolicy(context.Background(), "web", "locked"); err != nil {
		t.Fatalf("AttachPolicy: %v", err)
	}
	l.repo.left = []models.Sandbox{l.repo.sb}

	holders, err := sandbox.PolicyHolders(l.repo, "locked")
	if err != nil {
		t.Fatalf("PolicyHolders: %v", err)
	}
	if !slices.Equal(holders, []string{"sandbox1"}) {
		t.Errorf("PolicyHolders = %v", holders)
	}
}
