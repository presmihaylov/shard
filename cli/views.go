package cli

import (
	"strings"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/serve"
)

// imageView is what image list --format json prints: never the whole image, which also names its rootfs, disks and config.
type imageView struct {
	Reference string    `json:"reference"`
	Digest    string    `json:"digest"`
	Size      int64     `json:"size"`
	Created   time.Time `json:"created"`
	Broken    string    `json:"broken,omitempty"`
}

func imageViews(images []client.Image) []imageView {
	views := make([]imageView, 0, len(images))
	for _, img := range images {
		views = append(views, imageView{Reference: img.Reference, Digest: img.Digest, Size: img.Size, Created: img.Created, Broken: img.Broken})
	}

	return views
}

type policySummary struct {
	Name      string `json:"name"`
	RuleCount int    `json:"rule_count"`
}

func policySummaries(policies []models.Policy) []policySummary {
	summaries := make([]policySummary, 0, len(policies))
	for _, policy := range policies {
		summaries = append(summaries, policySummary{Name: policy.Name, RuleCount: len(policy.Rules)})
	}

	return summaries
}

// tokenView spells the ledger entry the way the table names its columns, and a token with no expiry is null.
type tokenView struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	IssuedAt  time.Time         `json:"issued_at"`
	ExpiresAt *time.Time        `json:"expires_at"`
	Scopes    []string          `json:"scopes"`
	Status    serve.TokenStatus `json:"status"`
}

func tokenViews(infos []serve.TokenInfo) []tokenView {
	views := make([]tokenView, 0, len(infos))
	for _, info := range infos {
		views = append(views, tokenView{ID: info.ID, Name: info.Subject, IssuedAt: info.IssuedAt, ExpiresAt: info.ExpiresAt, Scopes: nonNil(info.Scopes), Status: info.Status})
	}

	return views
}

type infoView struct {
	Provider   string `json:"provider"`
	Reason     string `json:"reason"`
	Unreadable string `json:"unreadable,omitempty"`
}

type versionView struct {
	Client string `json:"client"`
	Daemon string `json:"daemon"`
	Shim   string `json:"shim,omitempty"`
}

// inspectSections is the record read down a page, then the rules the host enforces for it.
func inspectSections(record any, insp sandbox.Inspection) ([]section, error) {
	fields, err := fieldSection(record)
	if err != nil {
		return nil, err
	}
	if insp.Egress == nil {
		return []section{fields}, nil
	}

	fields.rows = append(fields.rows, []string{"egress.policy", orDash(insp.Egress.Policy)})
	if insp.Egress.Missing {
		fields.rows = append(fields.rows, []string{"egress.missing", "true"})
	}

	rules := section{columns: []string{"ID", "RULE", "IMPLIED"}}
	for _, rule := range insp.Egress.Rules {
		rules.rows = append(rules.rows, []string{rule.ID, client.FormatRule(rule.Rule), orDash(rule.Implied)})
	}

	return []section{fields, rules}, nil
}

func policySections(policy client.PolicyView) []section {
	fields := section{columns: []string{"FIELD", "VALUE"}, rows: [][]string{
		{"name", policy.Name},
		{"dns", policy.DNS},
		{"holders", orDash(strings.Join(policy.Holders, ","))},
	}}

	rules := section{columns: []string{"RULE"}}
	for _, rule := range policy.Rules {
		rules.rows = append(rules.rows, []string{client.FormatRule(rule)})
	}

	return []section{fields, rules}
}

func mintSection(minted serve.Token) section {
	return section{
		columns: []string{"TOKEN", "EXPIRES", "SCOPES"},
		rows:    [][]string{{minted.Token, expiresText(minted.ExpiresAt), strings.Join(minted.Scopes, ",")}},
	}
}
