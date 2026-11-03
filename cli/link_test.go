package cli

import (
	"bytes"
	"encoding/json"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
)

func TestCreateTakesRepeatedLinks(t *testing.T) {
	req, err := parseCreate([]string{"--link", "web1", "--link", "db1", "alpine:3.20"})
	if want := []string{"web1", "db1"}; err != nil || !slices.Equal(req.Links, want) {
		t.Errorf("parseCreate = %q, %v; want the links %q", req.Links, err, want)
	}

	if _, err := parseCreate([]string{"--link", "web1", "--link", "web1", "alpine:3.20"}); err == nil || !strings.Contains(err.Error(), "sandbox web1 is linked twice") {
		t.Errorf("parseCreate of one link twice = %v, want a refusal", err)
	}
	if _, err := parseCreate([]string{"--link", "", "alpine:3.20"}); err == nil || !strings.Contains(err.Error(), "got an empty one") {
		t.Errorf("parseCreate of an empty link = %v, want a refusal", err)
	}
}

func TestLinkVerbsKeepTheLinkOnTheRecord(t *testing.T) {
	var out bytes.Buffer
	app, d := newLifecycleApp(t, &out, &recorder{}, stopped())
	repo := d.repoSvc.(*fakeLifecycleRepo)
	repo.created = models.Sandbox{ID: "sandbox2", Name: "db1", State: models.StateStopped}

	if err := app.Run(t.Context(), []string{"link", "add", "web", "sandbox2"}); err != nil {
		t.Fatalf("link add: %v", err)
	}
	want := []models.Link{{Target: "db1", TargetID: "sandbox2", Address: netip.MustParseAddr("10.86.0.1")}}
	if !slices.Equal(repo.sb.Links, want) {
		t.Errorf("the record holds %+v, want %+v", repo.sb.Links, want)
	}
	if strings.Join(strings.Fields(out.String()), " ") != "db1 10.86.0.1" {
		t.Errorf("link add printed %q, want the target and its address", out.String())
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"link", "ls", "web"}); err != nil {
		t.Fatalf("link ls: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || strings.Join(strings.Fields(lines[0]), " ") != "TARGET ADDRESS" || strings.Join(strings.Fields(lines[1]), " ") != "db1 10.86.0.1" {
		t.Errorf("link ls printed %q", out.String())
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"link", "list", "--format", "json", "web"}); err != nil {
		t.Fatalf("link list --format json: %v", err)
	}
	var rows []models.Link
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil || !slices.Equal(rows, want) {
		t.Errorf("link list --format json printed %s (%v)", out.String(), err)
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"inspect", "--format", "table", "web"}); err != nil {
		t.Fatalf("inspect: %v", err)
	}
	for _, row := range []string{"links[0].target db1", "links[0].target_id sandbox2", "links[0].address 10.86.0.1"} {
		if !strings.Contains(strings.Join(strings.Fields(out.String()), " "), row) {
			t.Errorf("inspect printed %q, want the row %q", out.String(), row)
		}
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"link", "rm", "web", "db1"}); err != nil || out.String() != "db1\n" {
		t.Fatalf("link rm printed %q, %v; want the target", out.String(), err)
	}
	if len(repo.sb.Links) != 0 {
		t.Errorf("the record still holds %+v", repo.sb.Links)
	}

	err := app.Run(t.Context(), []string{"link", "remove", "web", "db1"})
	if err == nil || !strings.Contains(err.Error(), "sandbox web holds no link to db1") {
		t.Errorf("a second link remove returned %v, want the link named missing", err)
	}
}

func TestLinkVerbsRefuseTheWrongArguments(t *testing.T) {
	var out bytes.Buffer
	app, _ := newLifecycleApp(t, &out, &recorder{}, stopped())

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"link", "add", "web"}, "link add takes a sandbox and the sandbox it links"},
		{[]string{"link", "rm", "web", "db1", "cache"}, "link remove takes a sandbox and the sandbox it links"},
		{[]string{"link", "ls"}, "link list takes one sandbox"},
		{[]string{"link", "add", "web", "web"}, "sandbox web cannot link itself"},
	} {
		if err := app.Run(t.Context(), tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q returned %v, want %q", tc.args, err, tc.want)
		}
	}
}

func TestLinkListOfNoLinksPrintsAnEmptyArray(t *testing.T) {
	var out bytes.Buffer
	app, _ := newLifecycleApp(t, &out, &recorder{}, stopped())

	if err := app.Run(t.Context(), []string{"link", "list", "--format", "json", "web"}); err != nil || strings.TrimSpace(out.String()) != "[]" {
		t.Errorf("link list --format json printed %q, %v; want []", out.String(), err)
	}
}
