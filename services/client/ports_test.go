package client_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/sandbox"
)

func TestThePortVerbsNameTheRoute(t *testing.T) {
	var saw seen
	c := serve(t, shortRoot(t), echo(http.StatusOK, `{"sandbox":"sandbox1","host_port":8080,"guest_port":80,"public":true,"address":"0.0.0.0","listening":true,"reachable_on":[]}`, &saw))

	row, err := c.AddPort(t.Context(), "web", 8080, sandbox.PortRequest{GuestPort: 80, Public: true})
	if err != nil || row.HostPort != 8080 || row.GuestPort != 80 || !row.Listening {
		t.Fatalf("AddPort = %+v, %v; want 8080 to 80, listening", row, err)
	}
	if saw.method != http.MethodPut || saw.uri != "/v0/sandboxes/web/ports/8080" || string(saw.body) != `{"guest_port":80,"public":true}` {
		t.Errorf("the add was %s %s with %q, want PUT /v0/sandboxes/web/ports/8080 with the guest port", saw.method, saw.uri, saw.body)
	}

	c = serve(t, shortRoot(t), echo(http.StatusOK, `{"ports":[{"host_port":8080},{"host_port":9090}],"next":null}`, &saw))
	rows, err := c.ListPorts(t.Context(), "")
	if err != nil || len(rows) != 2 || rows[1].HostPort != 9090 {
		t.Fatalf("ListPorts = %+v, %v; want 8080 and 9090", rows, err)
	}
	if saw.method != http.MethodGet || saw.uri != "/v0/ports" {
		t.Errorf("the list of every forward was %s %s, want GET /v0/ports", saw.method, saw.uri)
	}

	if _, err := c.ListPorts(t.Context(), "web"); err != nil || saw.uri != "/v0/sandboxes/web/ports" {
		t.Errorf("the list of one sandbox was GET %s, %v; want GET /v0/sandboxes/web/ports", saw.uri, err)
	}

	c = serve(t, shortRoot(t), echo(http.StatusNoContent, ``, &saw))
	if err := c.RemovePort(t.Context(), "web", 8080); err != nil {
		t.Fatalf("RemovePort = %v", err)
	}
	if saw.method != http.MethodDelete || saw.uri != "/v0/sandboxes/web/ports/8080" {
		t.Errorf("the remove was %s %s, want DELETE /v0/sandboxes/web/ports/8080", saw.method, saw.uri)
	}
}

// NotFoundError prints "no sandbox", so a host port the sandbox does not forward keeps the daemon's own line.
func TestRemovingAPortTheSandboxDoesNotForwardKeepsTheDaemonLine(t *testing.T) {
	const line = "sandbox web forwards no host port 8080: shard port list web lists its forwards"
	c := serve(t, shortRoot(t), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			answer(http.StatusOK, `{"id":"sandbox1","name":"web"}`)(w, r)

			return
		}
		answer(http.StatusNotFound, `{"error":{"code":"not_found","message":"`+line+`"}}`)(w, r)
	})

	err := c.RemovePort(t.Context(), "web", 8080)

	var notFound *client.NotFoundError
	var refusal *client.APIError
	if errors.As(err, &notFound) || !errors.As(err, &refusal) || refusal.Message != line {
		t.Errorf("RemovePort = %v, want the APIError %q", err, line)
	}
}

func TestRemovingAPortOfASandboxThatIsNotThereNamesTheSandbox(t *testing.T) {
	c := serve(t, shortRoot(t), answer(http.StatusNotFound, `{"error":{"code":"not_found","message":"sandbox ghost not found"}}`))

	err := c.RemovePort(t.Context(), "ghost", 8080)

	var notFound *client.NotFoundError
	if !errors.As(err, &notFound) || notFound.Ref != "ghost" {
		t.Errorf("RemovePort = %v, want a NotFoundError for ghost", err)
	}
}
