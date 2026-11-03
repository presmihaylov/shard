package client_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/presmihaylov/shard/services/client"
)

func TestTheLinkVerbsNameTheRoute(t *testing.T) {
	var saw seen
	c := serve(t, shortRoot(t), echo(http.StatusOK, `{"target":"web1","target_id":"sandbox2","address":"10.86.0.1"}`, &saw))

	link, err := c.AddLink(t.Context(), "agent1", "web1")
	if err != nil || link.Target != "web1" || link.Address.String() != "10.86.0.1" {
		t.Fatalf("AddLink = %+v, %v; want web1 at 10.86.0.1", link, err)
	}
	if saw.method != http.MethodPut || saw.uri != "/v0/sandboxes/agent1/links/web1" {
		t.Errorf("the add was %s %s, want PUT /v0/sandboxes/agent1/links/web1", saw.method, saw.uri)
	}

	c = serve(t, shortRoot(t), echo(http.StatusOK, `{"links":[{"target":"web1","address":"10.86.0.1"},{"target":"db1","address":"10.86.0.2"}]}`, &saw))
	links, err := c.ListLinks(t.Context(), "agent1")
	if err != nil || len(links) != 2 || links[1].Target != "db1" {
		t.Fatalf("ListLinks = %+v, %v; want web1 and db1", links, err)
	}
	if saw.method != http.MethodGet || saw.uri != "/v0/sandboxes/agent1/links" {
		t.Errorf("the list was %s %s, want GET /v0/sandboxes/agent1/links", saw.method, saw.uri)
	}

	c = serve(t, shortRoot(t), echo(http.StatusNoContent, ``, &saw))
	if err := c.RemoveLink(t.Context(), "agent1", "web1"); err != nil {
		t.Fatalf("RemoveLink = %v", err)
	}
	if saw.method != http.MethodDelete || saw.uri != "/v0/sandboxes/agent1/links/web1" {
		t.Errorf("the remove was %s %s, want DELETE /v0/sandboxes/agent1/links/web1", saw.method, saw.uri)
	}
}

// NotFoundError prints "no sandbox", so a link the sandbox does not hold keeps the daemon's own line.
func TestRemovingALinkTheSandboxDoesNotHoldKeepsTheDaemonLine(t *testing.T) {
	const line = "sandbox agent1 holds no link to web1: shard link list agent1 lists its links"
	c := serve(t, shortRoot(t), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			answer(http.StatusOK, `{"id":"sandbox1","name":"agent1"}`)(w, r)

			return
		}
		answer(http.StatusNotFound, `{"error":{"code":"not_found","message":"`+line+`"}}`)(w, r)
	})

	err := c.RemoveLink(t.Context(), "agent1", "web1")

	var notFound *client.NotFoundError
	var refusal *client.APIError
	if errors.As(err, &notFound) || !errors.As(err, &refusal) || refusal.Message != line {
		t.Errorf("RemoveLink = %v, want the APIError %q", err, line)
	}
}

func TestLinkingFromASandboxThatIsNotThereNamesTheSandbox(t *testing.T) {
	c := serve(t, shortRoot(t), answer(http.StatusNotFound, `{"error":{"code":"not_found","message":"sandbox ghost not found"}}`))

	_, err := c.AddLink(t.Context(), "ghost", "web1")

	var notFound *client.NotFoundError
	if !errors.As(err, &notFound) || notFound.Ref != "ghost" {
		t.Errorf("AddLink = %v, want a NotFoundError for ghost", err)
	}
}
