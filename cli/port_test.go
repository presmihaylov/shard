package cli

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
)

func TestParseForwardReadsHostAndGuestPorts(t *testing.T) {
	for text, want := range map[string]models.PortForward{
		"8100":      {HostPort: 8100, GuestPort: 8100},
		"9000:8100": {HostPort: 9000, GuestPort: 8100},
		"1:65535":   {HostPort: 1, GuestPort: 65535},
	} {
		got, err := parseForward(text, "")
		if err != nil || got != want {
			t.Errorf("parseForward(%q) = %+v, %v; want %+v", text, got, err, want)
		}
	}
}

func TestParseForwardRefusesAnAddressAndPointsToPublic(t *testing.T) {
	for _, text := range []string{"0.0.0.0:9000:8100", "127.0.0.1:9000", "1:2:3", "localhost.:80"} {
		_, err := parseForward(text, "use --public to bind 0.0.0.0")
		if err == nil || !strings.Contains(err.Error(), "use --public to bind 0.0.0.0") {
			t.Errorf("parseForward(%q) = %v, want the --public hint", text, err)
		}
	}
	for _, text := range []string{"0", "65536", "http", "9000:", ":80", "-1", ""} {
		if _, err := parseForward(text, ""); err == nil || !strings.Contains(err.Error(), "from 1 to 65535") {
			t.Errorf("parseForward(%q) = %v, want a refusal of the port number", text, err)
		}
	}
}

// The spec spells --public after the ports, which the flag package alone would take for a third argument.
func TestPortAddTakesPublicOnEitherSideOfTheArguments(t *testing.T) {
	want := portAddOptions{ref: "web", forward: models.PortForward{HostPort: 9000, GuestPort: 8100, Public: true}}
	for _, args := range [][]string{
		{"web", "9000:8100", "--public"},
		{"--public", "web", "9000:8100"},
		{"web", "--public", "9000:8100"},
	} {
		got, err := parsePortAdd(args)
		if err != nil || got != want {
			t.Errorf("parsePortAdd(%q) = %+v, %v; want %+v", args, got, err, want)
		}
	}

	got, err := parsePortAdd([]string{"--", "-web", "8100"})
	if err != nil || got.ref != "-web" || got.forward.Public {
		t.Errorf("parsePortAdd(-- -web 8100) = %+v, %v; want the private forward of sandbox -web", got, err)
	}

	for _, args := range [][]string{{"web"}, {"web", "80", "81"}, {"web", "--", "80", "--public"}} {
		if _, err := parsePortAdd(args); err == nil || !strings.Contains(err.Error(), "port add takes a sandbox and a [HOST:]GUEST port") {
			t.Errorf("parsePortAdd(%q) = %v, want the usage", args, err)
		}
	}
}

func TestCreateTakesRepeatedPrivatePorts(t *testing.T) {
	req, err := parseCreate([]string{"--port", "8000", "--port", "9000:80", "alpine:3.20"})
	if want := []models.PortForward{{HostPort: 8000, GuestPort: 8000}, {HostPort: 9000, GuestPort: 80}}; err != nil || !slices.Equal(req.Ports, want) {
		t.Errorf("parseCreate = %+v, %v; want the forwards %+v", req.Ports, err, want)
	}

	if _, err := parseCreate([]string{"--port", "9000:80", "--port", "9000:81", "alpine:3.20"}); err == nil || !strings.Contains(err.Error(), "host port 9000 is given twice") {
		t.Errorf("parseCreate of one host port twice = %v, want a refusal", err)
	}
	if _, err := parseCreate([]string{"--port", "0.0.0.0:9000:80", "alpine:3.20"}); err == nil || !strings.Contains(err.Error(), "shard port add --public") {
		t.Errorf("parseCreate of an address = %v, want the pointer to port add --public", err)
	}
}

func TestPortVerbsKeepTheForwardOnTheRecord(t *testing.T) {
	var out, errOut bytes.Buffer
	app, d := newLifecycleApp(t, &out, &recorder{}, stopped())
	app.Err = &errOut

	if err := app.Run(t.Context(), []string{"port", "add", "web", "9000:8000", "--public"}); err != nil {
		t.Fatalf("port add: %v", err)
	}
	repo := d.repoSvc.(*fakeLifecycleRepo)
	if want := []models.PortForward{{HostPort: 9000, GuestPort: 8000, Public: true}}; !slices.Equal(repo.sb.Ports, want) {
		t.Errorf("the record holds %+v, want %+v", repo.sb.Ports, want)
	}
	// A stopped sandbox listens on nothing, so stdout has no address to name and stderr says why.
	if out.String() != "" || !strings.Contains(errOut.String(), "sandbox web is not running") || !strings.Contains(errOut.String(), "every interface") {
		t.Errorf("port add printed %q and %q, want no address and the two notes", out.String(), errOut.String())
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"port", "ls", "web"}); err != nil {
		t.Fatalf("port ls: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || strings.Join(strings.Fields(lines[0]), " ") != "SANDBOX HOST GUEST VISIBILITY REACHABLE-ON" || strings.Join(strings.Fields(lines[1]), " ") != "web 9000 8000 public -" {
		t.Errorf("port ls printed %q", out.String())
	}

	repo.left = []models.Sandbox{repo.sb}
	out.Reset()
	if err := app.Run(t.Context(), []string{"port", "list", "--format", "json"}); err != nil {
		t.Fatalf("port list --format json: %v", err)
	}
	var rows []models.Port
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil || len(rows) != 1 || rows[0].Sandbox != "sandbox1" || rows[0].Address != "0.0.0.0" {
		t.Errorf("port list --format json printed %s (%v)", out.String(), err)
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"port", "rm", "web", "9000"}); err != nil || out.String() != "9000\n" {
		t.Fatalf("port rm printed %q, %v; want the host port", out.String(), err)
	}
	if len(repo.sb.Ports) != 0 {
		t.Errorf("the record still holds %+v", repo.sb.Ports)
	}

	err := app.Run(t.Context(), []string{"port", "remove", "web", "9000"})
	if err == nil || !strings.Contains(err.Error(), "sandbox web forwards no host port 9000") {
		t.Errorf("a second port remove returned %v, want the forward named missing", err)
	}
}

func TestPortListOfNoForwardsPrintsAnEmptyArray(t *testing.T) {
	var out bytes.Buffer
	app, _ := newLifecycleApp(t, &out, &recorder{}, stopped())

	if err := app.Run(t.Context(), []string{"port", "list", "--format", "json"}); err != nil || strings.TrimSpace(out.String()) != "[]" {
		t.Errorf("port list --format json printed %q, %v; want []", out.String(), err)
	}
}

func TestPrintForwardNamesEachAddressOfTheHost(t *testing.T) {
	var out, errOut bytes.Buffer
	app := App{Out: &out, Err: &errOut}

	err := app.printForward(models.Port{Sandbox: "sandbox1", SandboxName: "web", HostPort: 9000, GuestPort: 8000, Public: true, Listening: true,
		ReachableOn: []models.HostAddress{{Interface: "lo0", Address: "127.0.0.1"}, {Interface: "en0", Address: "192.0.2.20"}}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "lo0   127.0.0.1:9000\nen0   192.0.2.20:9000\n"; out.String() != want {
		t.Errorf("port add printed %q, want %q", out.String(), want)
	}
	if strings.Contains(errOut.String(), "not running") {
		t.Errorf("a listening forward printed %q", errOut.String())
	}
}
