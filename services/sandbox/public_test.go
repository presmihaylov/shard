package sandbox_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

// hostCause carries what no public text may: a host path under the root and a pid.
const hostCause = "runsc start: open /var/lib/shard/sandboxes/sb1/config.json: pid 4242: permission denied"

type auditedError struct{}

func (auditedError) Error() string {
	return "the image ref is not valid: read /var/lib/shard/images/index.json"
}

func (auditedError) Public() string { return "the image ref is not valid" }

func TestPublicTextIsTheFirstOptInUnderAWrapper(t *testing.T) {
	err := fmt.Errorf("create sb1 under /var/lib/shard: %w", auditedError{})

	public, ok := sandbox.PublicText(err)
	if !ok || public != "the image ref is not valid" {
		t.Errorf("public text = %q, %v, want the opt-in's own words", public, ok)
	}
}

func TestPublicTextFindsAnOptInJoinedWithARawCause(t *testing.T) {
	err := errors.Join(errors.New(hostCause), fmt.Errorf("pid 4242: %w", auditedError{}))

	public, ok := sandbox.PublicText(err)
	if !ok || public != "the image ref is not valid" {
		t.Errorf("public text = %q, %v, want the opt-in's own words", public, ok)
	}
}

func TestPublicTextOfARawCauseIsAbsent(t *testing.T) {
	if public, ok := sandbox.PublicText(fmt.Errorf("start sb1: %w", errors.New(hostCause))); ok {
		t.Errorf("public text = %q, want none for a cause that does not opt in", public)
	}
}

func TestCauseErrorKeepsTheCauseOutOfItsPublicText(t *testing.T) {
	raw := &sandbox.CauseError{Text: "cannot tell which sandboxes hold the secret", Err: errors.New(hostCause)}
	if got := raw.Public(); got != "cannot tell which sandboxes hold the secret; the daemon log has the cause" {
		t.Errorf("public text = %q, want the refusal and a pointer to the log", got)
	}
	if !strings.Contains(raw.Error(), hostCause) {
		t.Errorf("text = %q, want the whole cause for the log", raw.Error())
	}

	audited := &sandbox.CauseError{Text: "cannot tell which sandboxes hold the secret", Err: fmt.Errorf("wrap: %w", auditedError{})}
	if got := audited.Public(); got != "cannot tell which sandboxes hold the secret: the image ref is not valid" {
		t.Errorf("public text = %q, want the refusal and its cause's public words", got)
	}
}

// oldFailed is a record written before failed_public existed: its only reason is the raw cause.
func oldFailed() models.Sandbox {
	return models.Sandbox{ID: "sb1", State: models.StateFailed, FailedReason: hostCause}
}

func TestPublicReasonOfAnOldRecordIsTheGenericText(t *testing.T) {
	if got := sandbox.PublicReason(oldFailed()); got != sandbox.FailedGeneric {
		t.Errorf("public reason = %q, want the generic text", got)
	}

	current := oldFailed()
	current.FailedPublic = "the image ref is not valid"
	if got := sandbox.PublicReason(current); got != current.FailedPublic {
		t.Errorf("public reason = %q, want the record's public text", got)
	}

	if got := sandbox.PublicReason(models.Sandbox{ID: "sb1", State: models.StateRunning}); got != "" {
		t.Errorf("public reason of a sandbox that never failed = %q, want none", got)
	}
}

func TestFailedGuardOfAnOldRecordAnswersTheGenericText(t *testing.T) {
	err := sandbox.FailedGuard("sb1", oldFailed())

	var refused *sandbox.StateError
	if !errors.As(err, &refused) || refused.Code != models.CodeSandboxFailed {
		t.Fatalf("guard = %v, want a sandbox_failed refusal", err)
	}
	if public := refused.Public(); !strings.Contains(public, sandbox.FailedGeneric) || strings.Contains(public, "/var/lib/shard") || strings.Contains(public, "4242") {
		t.Errorf("public text = %q, want the generic text and no host detail", public)
	}
	if !strings.Contains(refused.Error(), hostCause) {
		t.Errorf("text = %q, want the raw cause for a local route and the log", refused.Error())
	}
}

// startProvider fails every start with err, to put a chosen cause on a failed record.
type startProvider struct {
	models.Provider

	err error
}

func (p *startProvider) Start(context.Context, string) error { return p.err }

func failStart(err error) func(*sandbox.Config) {
	return func(c *sandbox.Config) { c.Provider = &startProvider{Provider: c.Provider, err: err} }
}

const syntheticValue = "sk_live_synthetic_0001"

func redactSynthetic(c *sandbox.Config) {
	c.Redact = func(text string) string { return strings.ReplaceAll(text, syntheticValue, "<secret API_KEY>") }
}

func TestAFailedCreateKeepsTheCauseLocalAndNoSecretValue(t *testing.T) {
	cause := fmt.Errorf("%s: env API_KEY=%s", hostCause, syntheticValue)
	svc, l := newService(t, &recorder{}, models.Sandbox{}, failStart(cause), redactSynthetic)

	if _, err := svc.Create(t.Context(), alpine()); err == nil {
		t.Fatal("a failed start returned no error")
	}

	sb := l.repo.sb
	if sb.State != models.StateFailed || sb.FailedPublic != sandbox.FailedGeneric {
		t.Fatalf("the record is %s with public reason %q, want failed with the generic text", sb.State, sb.FailedPublic)
	}
	if !strings.Contains(sb.FailedReason, "/var/lib/shard") || !strings.Contains(sb.FailedReason, "<secret API_KEY>") {
		t.Errorf("failed_reason = %q, want the raw cause with the secret's name", sb.FailedReason)
	}
	if strings.Contains(sb.FailedReason, syntheticValue) || strings.Contains(sb.FailedPublic, syntheticValue) {
		t.Errorf("the record holds the secret value: %q, %q", sb.FailedReason, sb.FailedPublic)
	}
}

func TestAFailedCreateAnswersTheCausesPublicText(t *testing.T) {
	timeout := &sandbox.SubstrateTimeoutError{ID: "sb1", Op: "start", Budget: time.Second}
	svc, l := newService(t, &recorder{}, models.Sandbox{}, failStart(fmt.Errorf("start under /var/lib/shard pid 4242: %w", timeout)))

	if _, err := svc.Create(t.Context(), alpine()); err == nil {
		t.Fatal("a failed start returned no error")
	}

	if got := l.repo.sb.FailedPublic; got != timeout.Public() {
		t.Errorf("failed_public = %q, want the timeout's public text %q", got, timeout.Public())
	}
}
