package api_test

import (
	"net/http"
	"slices"
	"syscall"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/portforward"
	"github.com/presmihaylov/shard/services/sandbox"
)

func TestPortPutAndRemoveNameTheSandboxAndTheHostPort(t *testing.T) {
	s := seed(t)

	status, got := send(t, s.server, http.MethodPut, "/v0/sandboxes/web/ports/8080", `{"guest_port":80,"public":true}`)
	if status != http.StatusOK || s.verbs.ref != "web" || s.verbs.hostPort != 8080 || s.verbs.port != (sandbox.PortRequest{GuestPort: 80, Public: true}) {
		t.Errorf("the put answered %d %v over ref %q, host port %d and %+v", status, got, s.verbs.ref, s.verbs.hostPort, s.verbs.port)
	}
	if got["host_port"] != float64(8080) || got["guest_port"] != float64(80) {
		t.Errorf("the put answered the row %v, want host port 8080 to guest port 80", got)
	}

	s.verbs.hostPort = 0
	status, got = send(t, s.server, http.MethodDelete, "/v0/sandboxes/web/ports/8080", "")
	if status != http.StatusNoContent || s.verbs.hostPort != 8080 {
		t.Errorf("the rm answered %d %v over host port %d, want 204 over 8080", status, got, s.verbs.hostPort)
	}
}

func TestPortPutRefusesWhatIsNoPortBeforeTheVerb(t *testing.T) {
	s := seed(t)

	for _, req := range []struct{ path, body string }{
		{"/v0/sandboxes/web/ports/0", `{"guest_port":80}`},
		{"/v0/sandboxes/web/ports/65536", `{"guest_port":80}`},
		{"/v0/sandboxes/web/ports/http", `{"guest_port":80}`},
		{"/v0/sandboxes/web/ports/8080", `{"guest_port":0}`},
		{"/v0/sandboxes/web/ports/8080", `{"guest_port":65536}`},
		{"/v0/sandboxes/web/ports/8080", `{}`},
	} {
		status, got := send(t, s.server, http.MethodPut, req.path, req.body)
		if status != http.StatusBadRequest || errorOf(t, got).code != "invalid_request" {
			t.Errorf("PUT %s %s answered %d %v, want 400 invalid_request", req.path, req.body, status, got)
		}
	}
	if s.verbs.hostPort != 0 {
		t.Errorf("AddPort ran on a refused request, over host port %d", s.verbs.hostPort)
	}
}

func TestPortListsPageInHostPortOrder(t *testing.T) {
	s := seed(t)
	s.verbs.ports = []models.Port{{Sandbox: "a", HostPort: 80}, {Sandbox: "b", HostPort: 9000}, {Sandbox: "a", HostPort: 10000}}

	status, got := get(t, s.server, "/v0/ports?limit=2")
	if status != http.StatusOK || !slices.Equal(hostPorts(t, got), []float64{80, 9000}) || next(t, got) != "9000" || s.verbs.ref != "" {
		t.Errorf("GET /v0/ports?limit=2 answered %d %v over ref %q, want 80 and 9000 of every sandbox and 9000 as next", status, got, s.verbs.ref)
	}

	// As text 10000 sorts before 9000, so a cursor compared as a string would end the list here.
	status, got = get(t, s.server, "/v0/sandboxes/web/ports?cursor=9000")
	if status != http.StatusOK || !slices.Equal(hostPorts(t, got), []float64{10000}) || next(t, got) != "" || s.verbs.ref != "web" {
		t.Errorf("GET /v0/sandboxes/web/ports?cursor=9000 answered %d %v over ref %q, want 10000 alone and a null next", status, got, s.verbs.ref)
	}

	for _, cursor := range []string{"http", "0", "65536"} {
		status, got := get(t, s.server, "/v0/ports?cursor="+cursor)
		if status != http.StatusBadRequest || errorOf(t, got).code != "invalid_request" {
			t.Errorf("GET /v0/ports?cursor=%s answered %d %v, want 400 invalid_request", cursor, status, got)
		}
	}
}

func TestPortListAnswersAnEmptyListAsAnArray(t *testing.T) {
	s := seed(t)

	status, got := get(t, s.server, "/v0/ports")
	rows, ok := got["ports"].([]any)
	if status != http.StatusOK || !ok || len(rows) != 0 {
		t.Errorf("GET /v0/ports answered %d %v, want an empty ports array", status, got)
	}
}

func TestPortRefusalsAnswerTheirCodes(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, body string
		err                      error
		status                   int
		code, message            string
	}{
		{"an unknown host port", http.MethodDelete, "/v0/sandboxes/web/ports/8080", "", &sandbox.PortNotFoundError{Sandbox: "web", HostPort: 8080}, http.StatusNotFound, "not_found", "sandbox web forwards no host port 8080: shard port list web lists its forwards"},
		{"a host port another process holds", http.MethodPut, "/v0/sandboxes/web/ports/8080", `{"guest_port":80}`, &sandbox.PortInUseError{Err: &portforward.BindError{Port: 8080, Err: syscall.EADDRINUSE}}, http.StatusConflict, "in_use", "host port 8080 is in use on the host: pick another host port"},
		{"a provider without forwards", http.MethodPut, "/v0/sandboxes/web/ports/8080", `{"guest_port":80}`, models.Unsupported("sysbox", models.VerbPort), http.StatusConflict, "unsupported", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := seed(t)
			s.verbs.err = tc.err

			status, got := send(t, s.server, tc.method, tc.path, tc.body)
			refusal := errorOf(t, got)
			if status != tc.status || refusal.code != tc.code {
				t.Errorf("%s %s answered %d %v, want %d %s", tc.method, tc.path, status, got, tc.status, tc.code)
			}
			if tc.message != "" && refusal.message != tc.message {
				t.Errorf("%s %s answered %q, want %q", tc.method, tc.path, refusal.message, tc.message)
			}
		})
	}
}

func TestPortPutOfAHostPortAnotherSandboxForwardsNamesThatSandbox(t *testing.T) {
	s := seed(t)
	s.verbs.err = &sandbox.HeldError{Subject: "host port 8080", Verb: "forwarded to", Noun: "sandbox", Users: []string{"api"}, Fix: "remove that forward first with shard port remove api 8080"}

	status, got := send(t, s.server, http.MethodPut, "/v0/sandboxes/web/ports/8080", `{"guest_port":80}`)
	refusal := errorOf(t, got)
	if status != http.StatusConflict || refusal.code != "in_use" || !slices.Equal(refusal.holders, []any{"api"}) {
		t.Errorf("the put answered %d %v, want 409 in_use held by api", status, got)
	}
}

func TestCreateForwardingAPortWithoutPortWriteIs403(t *testing.T) {
	s := seed(t)

	status, got := sendScoped(t, s, "sandbox:write,port:read", bodyWithPort)
	if status != http.StatusForbidden {
		t.Fatalf("POST answered %d %v, want 403", status, got)
	}
	assertForbidden(t, got, "port:write")
	if s.verbs.created.Image != "" {
		t.Fatalf("Create ran on a refused request: %+v", s.verbs.created)
	}

	status, got = sendScoped(t, s, "sandbox:write,port:write", bodyWithPort)
	if status != http.StatusCreated || len(s.verbs.created.Ports) != 1 {
		t.Errorf("POST with port:write answered %d %v and created %+v, want 201 with the forward", status, got, s.verbs.created)
	}
}

const bodyWithPort = `{"image":"alpine:3.20","ports":[{"host_port":8080,"guest_port":80}],"resources":{"memory_mib":512,"vcpus":2}}`

func hostPorts(t *testing.T, body map[string]any) []float64 {
	t.Helper()

	rows, ok := body["ports"].([]any)
	if !ok {
		t.Fatalf("the body holds no ports array: %v", body)
	}

	out := make([]float64, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.(map[string]any)["host_port"].(float64))
	}

	return out
}
