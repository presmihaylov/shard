//go:build integration

package cli

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"testing"
)

// SHARD-167: a cursor is a position in the list, so a page after a sandbox that was removed since still serves what follows it.
func TestListPagesPastASandboxRemovedBetweenTwoPages(t *testing.T) {
	app, out := newCreateApp(t)
	c := newRawClient(t, app)

	var mine []string
	for range 3 {
		id := create(t, app, out, "/bin/true")
		t.Cleanup(func() { cleanUp(t, app, id) })
		mine = append(mine, id)
	}
	slices.Sort(mine)

	whole, next := c.page("/v0/sandboxes?all=true")
	if next != "" {
		t.Fatalf("the whole list carries next %q, want null", next)
	}
	at := slices.Index(whole, mine[0])
	if at < 0 {
		t.Fatalf("the list %v lacks %s", whole, mine[0])
	}

	page, next := c.page("/v0/sandboxes?all=true&limit=" + strconv.Itoa(at+1))
	if !slices.Equal(page, whole[:at+1]) || next != mine[0] {
		t.Fatalf("the first page is %v with next %q, want %v ending at %s", page, next, whole[:at+1], mine[0])
	}

	if err := app.Run(t.Context(), []string{"rm", "--force", mine[0]}); err != nil {
		t.Fatalf("rm --force: %v", err)
	}

	page, next = c.page("/v0/sandboxes?all=true&cursor=" + mine[0])
	if !slices.Equal(page, whole[at+1:]) || next != "" {
		t.Errorf("the page after the removed sandbox is %v with next %q, want %v and null", page, next, whole[at+1:])
	}

	if status, code := c.plainGet("/v0/sandboxes?cursor=not/an-id"); status != http.StatusBadRequest || code != "invalid_request" {
		t.Errorf("a malformed cursor answered %d %s, want 400 invalid_request", status, code)
	}
}

// page lists over the socket and answers the ids in order beside next, "" for null.
func (c rawClient) page(path string) ([]string, string) {
	c.t.Helper()

	resp, err := c.http.Get("http://shard" + path)
	if err != nil {
		c.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()

	var body struct {
		Sandboxes []struct {
			ID string `json:"id"`
		} `json:"sandboxes"`
		Next *string `json:"next"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		c.t.Fatalf("decode the answer of GET %s: %v", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("GET %s answered %d", path, resp.StatusCode)
	}

	ids := make([]string, 0, len(body.Sandboxes))
	for _, sb := range body.Sandboxes {
		ids = append(ids, sb.ID)
	}
	if body.Next == nil {
		return ids, ""
	}

	return ids, *body.Next
}
