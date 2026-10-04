package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/serve"
)

// The JSON keeps the full digest and the raw size and time, and none of the store's paths or the image config.
func TestImageViewIsTheDeclaredFieldsOnly(t *testing.T) {
	created := time.Date(2026, 10, 4, 7, 0, 0, 0, time.UTC)
	digest := "sha256:" + strings.Repeat("ab", 32)
	images := []client.Image{{Reference: "python:3.12", Digest: digest, RootFS: "/var/lib/shard/rootfs", Disk: "/var/lib/shard/disk.ext4", Size: 1 << 20, Created: created}}

	var out bytes.Buffer
	if err := writeJSON(&out, imageViews(images)); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}

	var got []map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d images, want 1", len(got))
	}
	for _, key := range []string{"rootfs", "disk", "erofs", "config", "broken"} {
		if _, ok := got[0][key]; ok {
			t.Errorf("the view carries %q: %s", key, out.String())
		}
	}
	if got[0]["digest"] != digest || got[0]["size"] != float64(1<<20) || got[0]["created"] != "2026-10-04T07:00:00Z" {
		t.Errorf("got %v, want the full digest, the size in bytes and an RFC 3339 time", got[0])
	}
}

func TestNoImagesIsAnEmptyArray(t *testing.T) {
	var out bytes.Buffer
	if err := writeJSON(&out, imageViews(nil)); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}

	if out.String() != "[]\n" {
		t.Errorf("got %q, want an empty array", out.String())
	}
}

// A token with no expiry is null, not absent, and no scopes is an empty list, so jq reads every entry the same way.
func TestTokenViewSpellsNoExpiryAsNull(t *testing.T) {
	issued := time.Date(2026, 10, 4, 7, 0, 0, 0, time.UTC)
	infos := []serve.TokenInfo{{ID: "tok-1", Subject: "ci", IssuedAt: issued, Status: serve.StatusActive}}

	var out bytes.Buffer
	if err := writeJSON(&out, tokenViews(infos)); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}

	want := `[
  {
    "id": "tok-1",
    "name": "ci",
    "issued_at": "2026-10-04T07:00:00Z",
    "expires_at": null,
    "scopes": [],
    "status": "active"
  }
]
`
	if out.String() != want {
		t.Errorf("got\n%s\nwant\n%s", out.String(), want)
	}
}

func TestPolicySummaryCountsTheRules(t *testing.T) {
	rule := models.Rule{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationDomain, Value: "example.com"}}
	got := policySummaries([]models.Policy{{Name: "web", Rules: []models.Rule{rule, rule}}})

	if len(got) != 1 || got[0].Name != "web" || got[0].RuleCount != 2 {
		t.Errorf("got %+v, want web with 2 rules", got)
	}
}

// The table of policy show is its fields, then one rule per row in the grammar policy create takes.
func TestPolicyShowTableIsTheFieldsThenTheRules(t *testing.T) {
	policy := client.PolicyView{
		Policy: models.Policy{Name: "web", Rules: []models.Rule{
			{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationDomainSuffix, Value: "example.com"}, Protocol: "tcp", Ports: []int{443}},
		}},
		DNS: "open",
	}

	var out bytes.Buffer
	if err := writeSections(&out, policySections(policy)...); err != nil {
		t.Fatalf("writeSections: %v", err)
	}

	want := "FIELD     VALUE\nname      web\ndns       open\nholders   -\n\nRULE\nallow suffix:example.com tcp:443\n"
	if out.String() != want {
		t.Errorf("got\n%q\nwant\n%q", out.String(), want)
	}
}

// inspect as a table is the record read down a page, then the rules the host enforces, with who implied each one.
func TestInspectTableIsTheRecordThenTheRules(t *testing.T) {
	rule := models.Rule{Action: models.ActionDeny, Destination: models.Destination{Kind: models.DestinationCIDR, Value: "10.0.0.0/8"}}
	insp := sandbox.Inspection{
		Sandbox: models.Sandbox{ID: "s-1", Image: "python:3.12", State: models.StateRunning, Resources: models.Resources{MemoryMiB: 512}},
		Egress:  &egress.Effective{Policy: "web", Rules: []egress.EffectiveRule{{Rule: rule, ID: "r1", Implied: "private ranges"}}},
	}

	sections, err := inspectSections(insp)
	if err != nil {
		t.Fatalf("inspectSections: %v", err)
	}
	var out bytes.Buffer
	if err := writeSections(&out, sections...); err != nil {
		t.Fatalf("writeSections: %v", err)
	}

	for _, line := range []string{"id                     s-1", "resources.memory_mib   512", "egress.policy          web", "ID   RULE              IMPLIED", "r1   deny 10.0.0.0/8   private ranges"} {
		if !strings.Contains(out.String(), line+"\n") {
			t.Errorf("the table has no line %q:\n%s", line, out.String())
		}
	}
	if strings.Contains(out.String(), "egress.rules") {
		t.Errorf("the rules are in the field section too:\n%s", out.String())
	}
}

func TestInspectTableOfASandboxWithNoPolicyHasNoRuleSection(t *testing.T) {
	sections, err := inspectSections(sandbox.Inspection{Sandbox: models.Sandbox{ID: "s-1"}})
	if err != nil {
		t.Fatalf("inspectSections: %v", err)
	}

	if len(sections) != 1 {
		t.Errorf("got %d sections, want the field section alone", len(sections))
	}
}

func TestMintTableIsOneRow(t *testing.T) {
	expires := time.Date(2026, 10, 5, 7, 0, 0, 0, time.UTC)

	var out bytes.Buffer
	if err := writeSections(&out, mintSection(serve.Token{Token: "tkn", ExpiresAt: &expires, Scopes: []string{"exec", "image"}})); err != nil {
		t.Fatalf("writeSections: %v", err)
	}

	want := "TOKEN   EXPIRES                SCOPES\ntkn     2026-10-05T07:00:00Z   exec,image\n"
	if out.String() != want {
		t.Errorf("got\n%q\nwant\n%q", out.String(), want)
	}
}
