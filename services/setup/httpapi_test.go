package setup

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/pkg/term"
	"github.com/presmihaylov/shard/services/serve"
)

// setupKey is the key the fake shard tokens mint prints; no output of setup may show it.
const setupKey = "sk_test_0123456789abcdef"

const mintLine = shardBinary + " --root " + DataDir + " --remote  tokens mint"

func minted(key string) string {
	return `{"token":"` + key + `","expires_at":"2027-10-06T00:00:00Z","scopes":[]}` + "\n"
}

// apiServer stands in for shard serve and the daemon behind it, and answers the version only to key.
func apiServer(t *testing.T, key string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprint(w, `{"version":"v0.1.0","api_version":"v0"}`); err != nil {
			t.Errorf("answer the version: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	return srv.Listener.Addr().String()
}

// newAPIHost is a Linux host setup can give the HTTP API: sudo from u, gVisor pinned, the account tools there, and a mint that prints setupKey.
func newAPIHost(t *testing.T) *localHost {
	t.Helper()
	l := newLocalHost(t)
	l.env["SUDO_USER"] = "u"
	l.pinGVisor()
	l.write("/usr/sbin/groupadd", "#!/bin/sh\n")
	l.write("/usr/sbin/useradd", "#!/bin/sh\n")
	l.answer[mintLine] = minted(setupKey)

	return l
}

// withAccount makes getent find the shard group and user.
func (l *localHost) withAccount() {
	l.answer["getent group shard"] = "shard:x:999:\n"
	l.answer["getent passwd shard"] = "shard:x:999:999::/:/usr/sbin/nologin\n"
}

// apiUI is a first setup of gVisor that starts at boot and says yes to the HTTP API on address.
func apiUI(address string) *localUI {
	ui := newLocalUI("true", true, GVisor)
	ui.selects[AskHTTPAPI] = "true"
	ui.texts = map[Question]string{AskListen: address}

	return ui
}

func hidesTheKey(t *testing.T, l *localHost, ui *fakeUI, key string) {
	t.Helper()
	lines := slices.Concat(ui.printed, l.calls)
	for _, list := range ui.lists {
		lines = append(lines, list.marks...)
	}
	for _, line := range lines {
		if strings.Contains(line, key) {
			t.Fatalf("the API key shows in %q", line)
		}
	}
}

func (l *localHost) read(path string) string {
	l.t.Helper()
	data, err := os.ReadFile(filepath.Join(l.root, path))
	if err != nil {
		l.t.Fatalf("read %s: %v", path, err)
	}

	return string(data)
}

func called(calls []string, prefix string) int {
	return slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

func TestCheckListen(t *testing.T) {
	for address, want := range map[string]string{
		"127.0.0.1:7850":       "",
		"100.64.0.5:9000":      "",
		"[fd7a::1]:7850":       "",
		"[fe80::1%eth0]:7850":  "[fe80::1%eth0]:7850 names an IPv6 zone, which a client URL cannot carry; name an address without one",
		"localhost:7850":       `"localhost:7850" is not an IP address and port, such as 127.0.0.1:7850`,
		"127.0.0.1":            `"127.0.0.1" is not an IP address and port, such as 127.0.0.1:7850`,
		"0.0.0.0:7850":         "0.0.0.0:7850 listens on every network, and the HTTP API serves plain HTTP; name one address, such as 127.0.0.1:7850 or a VPN address",
		"[::]:7850":            "[::]:7850 listens on every network, and the HTTP API serves plain HTTP; name one address, such as 127.0.0.1:7850 or a VPN address",
		"127.0.0.1:80":         "port 80 needs root, and shard serve runs without it; choose a port from 1024 up",
		"127.0.0.1:1024":       "",
		"127.0.0.1:2376 extra": `"127.0.0.1:2376 extra" is not an IP address and port, such as 127.0.0.1:7850`,
	} {
		err := CheckListen(address)
		if want == "" && err != nil || want != "" && (err == nil || err.Error() != want) {
			t.Errorf("CheckListen(%q) = %v, want %q", address, err, want)
		}
	}
}

func TestLocalSetsUpTheHTTPAPI(t *testing.T) {
	l := newAPIHost(t)
	address := apiServer(t, setupKey)
	ui := apiUI(address)

	if err := (&Setup{Host: l.host(), UI: ui}).local(t.Context(), stay); err != nil {
		t.Fatalf("local = %v; printed %q", err, ui.printed)
	}

	if want := []Question{AskProvider, AskStartAtBoot, AskHTTPAPI, AskListen, AskConfirm}; !slices.Equal(ui.asked, want) {
		t.Fatalf("asked %v, want %v", ui.asked, want)
	}
	if !slices.Equal(ui.initials, []string{serve.DefaultListen}) {
		t.Fatalf("the address question starts at %q, want %s", ui.initials, serve.DefaultListen)
	}
	said(t, ui.fakeUI,
		"HTTP API:          http://"+address,
		"  Create the shard account, which runs the HTTP API.",
		"  Create an API key in /etc/shard/api-key.",
		"  Configure and start a systemd service for the HTTP API on http://"+address+".",
		"The HTTP API serves plain HTTP. Setup does not configure HTTPS.",
		"API requests fail when the daemon is stopped.",
		"HTTP API:     http://"+address,
		"API key file: /etc/shard/api-key",
		"  Read the key with: sudo cat /etc/shard/api-key",
		"  Connect a client through an SSH tunnel. On the client, run:",
		"    shard setup --remote http://"+address+" --save -y",
	)
	if checks := ui.lists[0].steps; checks[len(checks)-1] != apiCheckTitle {
		t.Fatalf("the checks are %q, want the HTTP API last", checks)
	}
	steps := ui.lists[1].steps
	finished(t, ui.lists[1])
	if account, service := slices.Index(steps, "Create the shard account"), slices.Index(steps, "Configure the background service"); account < 0 || account > service {
		t.Fatalf("the steps are %q, want the account before the daemon's service", steps)
	}
	if want := []string{verifyDaemonTitle, "Configure the HTTP API service", "Create the API key", "Start the HTTP API", verifyAPITitle}; !slices.Equal(steps[len(steps)-len(want):], want) {
		t.Fatalf("the steps are %q, want them to end %q", steps, want)
	}

	if unit := l.read(serveUnit); !strings.Contains(unit, "serve --signing-key-file /etc/shard/serve.secret --listen "+address+"\n") {
		t.Fatalf("the unit names no address:\n%s", unit)
	}
	if secret := l.read(signingKeyFile); len(strings.TrimSpace(secret)) != 64 {
		t.Fatalf("the signing key is %d bytes, want 64 hex digits", len(strings.TrimSpace(secret)))
	}
	if key := l.read("/etc/shard/api-key"); key != setupKey+"\n" {
		t.Fatal("the key file does not hold the minted key")
	}
	info, err := os.Stat(filepath.Join(l.root, "/etc/shard/api-key"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the key file is %v, %v; want 0600", info.Mode(), err)
	}
	m, _, err := LoadManifest(l.host())
	if err != nil || m.API != address || !slices.Contains(m.Files, Owned{Path: serveUnit, Kind: KindService}) {
		t.Fatalf("manifest = %+v, %v", m, err)
	}
	group, start := called(l.calls, "groupadd --system shard"), slices.Index(l.calls, "systemctl start shard.service")
	if group < 0 || start < group || called(l.calls, "useradd --system --no-create-home --shell /usr/sbin/nologin --gid shard shard") < 0 {
		t.Fatalf("calls = %q, want the account made before the daemon starts", l.calls)
	}
	for _, c := range []string{"systemctl enable shard-serve.service", "systemctl restart shard-serve.service", mintLine + " --name shard-setup --signing-key-file /etc/shard/serve.secret"} {
		if !slices.Contains(l.calls, c) {
			t.Fatalf("calls = %q, want %q", l.calls, c)
		}
	}
	hidesTheKey(t, l, ui.fakeUI, setupKey)
}

func TestTheHTTPAPIDefaultsToNo(t *testing.T) {
	l := newAPIHost(t)
	ui := newLocalUI("true", true, GVisor)

	if err := (&Setup{Host: l.host(), UI: ui}).local(t.Context(), stay); err != nil {
		t.Fatalf("local = %v; printed %q", err, ui.printed)
	}

	options := ui.options[AskHTTPAPI]
	if len(options) != 2 || options[0].Label != "Yes - Configure the API as a background service" || options[1].Label != "No  - Use local connections only" || options[0].Default || !options[1].Default {
		t.Fatalf("the HTTP API options are %+v, want Yes, then No as the default", options)
	}
	said(t, ui.fakeUI, "HTTP API:          No")
	if l.exists(serveUnit) || called(l.calls, "getent") >= 0 || called(l.calls, mintLine) >= 0 {
		t.Fatalf("a No set up the HTTP API: %q", l.calls)
	}
}

func TestManualStartupNeverAsksForTheHTTPAPI(t *testing.T) {
	l := newAPIHost(t)
	ui := newLocalUI("false", true, GVisor)

	if err := (&Setup{Host: l.host(), UI: ui}).local(t.Context(), stay); err != nil {
		t.Fatalf("local = %v; printed %q", err, ui.printed)
	}
	if want := []Question{AskProvider, AskStartAtBoot, AskConfirm}; !slices.Equal(ui.asked, want) {
		t.Fatalf("asked %v, want %v", ui.asked, want)
	}
}

// listenUI answers the address question from a queue, so a test can give a bad address and then a good one.
type listenUI struct {
	*localUI
	addresses []string
}

func (u *listenUI) Text(ctx context.Context, q Question, prompt, initial string) (string, error) {
	if q == AskListen {
		u.texts[q], u.addresses = u.addresses[0], u.addresses[1:]
	}

	return u.localUI.Text(ctx, q, prompt, initial)
}

func TestABadAddressIsAskedAgain(t *testing.T) {
	l := newAPIHost(t)
	address := apiServer(t, setupKey)
	ui := &listenUI{localUI: apiUI(""), addresses: []string{"0.0.0.0:7850", " " + address + " "}}

	if err := (&Setup{Host: l.host(), UI: ui}).local(t.Context(), stay); err != nil {
		t.Fatalf("local = %v; printed %q", err, ui.printed)
	}
	said(t, ui.fakeUI, "0.0.0.0:7850 listens on every network, and the HTTP API serves plain HTTP; name one address, such as 127.0.0.1:7850 or a VPN address.")
	if n := slices.Index(ui.asked, AskListen); n < 0 || ui.asked[n+1] != AskListen {
		t.Fatalf("asked %v, want the address twice", ui.asked)
	}
	if m, _, err := LoadManifest(l.host()); err != nil || m.API != address {
		t.Fatalf("manifest API = %q, %v; want the trimmed address", m.API, err)
	}
}

func TestATakenAddressStopsBeforeAnyChange(t *testing.T) {
	for _, c := range []struct {
		name  string
		errno syscall.Errno
		want  string
	}{
		{"port in use", syscall.EADDRINUSE, "Port 7850 is in use by another program. / Stop that program, or run shard setup again and choose another port."},
		{"address of another machine", syscall.EADDRNOTAVAIL, "10.9.8.7 is not an address of this machine. / Run shard setup again and choose an address this machine has, such as 127.0.0.1 or its VPN address."},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := newAPIHost(t)
			l.listenErr = &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", c.errno)}
			ui := apiUI("10.9.8.7:7850")

			err := (&Setup{Host: l.host(), UI: ui}).local(t.Context(), stay)
			if stopped, ok := errors.AsType[*StoppedError](err); !ok || stopped.Step != apiCheckTitle {
				t.Fatalf("local = %v, want a stop at the HTTP API check", err)
			}
			said(t, ui.fakeUI, "No installation changes were made.")
			checks := ui.lists[0]
			if got, want := checks.marks[len(checks.marks)-1], fmt.Sprintf("fail %d: %s", len(checks.steps)-1, c.want); got != want {
				t.Fatalf("last mark %q, want %q", got, want)
			}
			if l.exists(shardBinary) || l.exists(ManifestPath) || called(l.calls, "groupadd") >= 0 {
				t.Fatalf("a taken address still changed the host: %q", l.calls)
			}
		})
	}
}

// A key an uninstall left is kept when its signing key is there too, and replaced unasked when the key that signed it is gone.
func TestALeftKeyIsKeptOnlyWithItsSigningKey(t *testing.T) {
	for _, c := range []struct {
		name    string
		signing bool
		want    string
	}{
		{"with its signing key", true, "  Keep the API key in /etc/shard/api-key."},
		{"without its signing key", false, "  Create an API key in /etc/shard/api-key."},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := newAPIHost(t)
			l.write("/etc/shard/api-key", "sk_left_behind\n")
			key := setupKey
			if c.signing {
				l.write(signingKeyFile, strings.Repeat("ab", 32)+"\n")
				key = "sk_left_behind"
			}
			ui := apiUI(apiServer(t, key))
			ui.confirms[AskReplaceKey] = false

			if err := (&Setup{Host: l.host(), UI: ui}).local(t.Context(), stay); err != nil {
				t.Fatalf("local = %v; printed %q", err, ui.printed)
			}
			said(t, ui.fakeUI, c.want)
			if asked := slices.Contains(ui.asked, AskReplaceKey); asked != c.signing {
				t.Fatalf("asked %v; want the replace question only with a signing key", ui.asked)
			}
			if minted := called(l.calls, mintLine) >= 0; minted == c.signing {
				t.Fatalf("calls = %q; want a mint only without a signing key", l.calls)
			}
			if got := l.read("/etc/shard/api-key"); got != key+"\n" {
				t.Fatal("the key file does not hold the key setup should leave")
			}
			if c.signing && l.read(signingKeyFile) != strings.Repeat("ab", 32)+"\n" {
				t.Fatal("setup replaced a signing key that was there")
			}
		})
	}
}

// installedWithAPI is a first setup that answered No to the HTTP API, with the daemon running and an older version recorded.
func installedWithoutAPI(t *testing.T, l *localHost) Manifest {
	t.Helper()
	h := l.host()
	if err := (&Setup{Host: h, UI: newLocalUI("true", true, GVisor)}).local(t.Context(), stay); err != nil {
		t.Fatalf("local = %v", err)
	}
	m, _, err := LoadManifest(h)
	if err != nil {
		t.Fatal(err)
	}
	m.Version = "v0.0.9"
	if err := saveManifest(t.Context(), h, m); err != nil {
		t.Fatal(err)
	}
	l.calls = nil
	l.answer["systemctl is-active shard"] = "active\n"

	return m
}

func TestTheHTTPAPIRowSetsUpOverAnInstallation(t *testing.T) {
	l := newAPIHost(t)
	m := installedWithoutAPI(t, l)
	// sudo can warn on stderr, which reaches setup before the key.
	l.answer[mintLine] = "sudo: unable to resolve host box: Name or service not known\n" + minted(setupKey)
	address := apiServer(t, setupKey)
	ui := &fakeUI{selects: map[Question]string{AskExisting: ExistingHTTPAPI}, texts: map[Question]string{AskListen: address}, confirms: map[Question]bool{AskConfirm: true}}

	if err := (&Setup{Host: l.host(), UI: ui}).existing(t.Context(), Installation{Manifest: &m, Service: ServiceActive}); err != nil {
		t.Fatalf("existing = %v; printed %q", err, ui.printed)
	}

	if want := []Question{AskExisting, AskListen, AskConfirm}; !slices.Equal(ui.asked, want) {
		t.Fatalf("asked %v, want %v", ui.asked, want)
	}
	list := ui.lists[len(ui.lists)-1]
	finished(t, list)
	want := []string{"Create the shard account", "Restart the daemon", verifyDaemonTitle, "Configure the HTTP API service", "Create the API key", "Start the HTTP API", verifyAPITitle}
	if list.title != "Setting up the HTTP API" || !slices.Equal(list.steps, want) {
		t.Fatalf("%s: %q, want %q", list.title, list.steps, want)
	}
	said(t, ui, "HTTP API: Not set up", "Ready to set up the HTTP API",
		"  Restart the daemon, so the HTTP API can reach its socket. Your sandboxes keep running.",
		"The HTTP API is set up.", "API key file: /etc/shard/api-key")
	got, _, err := LoadManifest(l.host())
	if err != nil || got.API != address || got.Version != "v0.0.9" || !slices.Contains(got.Files, Owned{Path: serveUnit, Kind: KindService}) {
		t.Fatalf("manifest = %+v, %v; want the API recorded and the version kept", got, err)
	}
	group, restart, serve := called(l.calls, "groupadd"), slices.Index(l.calls, "systemctl restart shard"), slices.Index(l.calls, "systemctl restart shard-serve.service")
	if group < 0 || restart < group || serve < restart {
		t.Fatalf("calls = %q, want the account, then the daemon restart, then the HTTP API", l.calls)
	}
	hidesTheKey(t, l, ui, setupKey)
}

func TestTheHTTPAPIRowKeepsOrReplacesTheKey(t *testing.T) {
	l := newAPIHost(t)
	l.withAccount()
	m := installedWithoutAPI(t, l)
	first := apiServer(t, setupKey)
	setUp := func(address string, replace bool) *fakeUI {
		t.Helper()
		ui := &fakeUI{texts: map[Question]string{AskListen: address}, confirms: map[Question]bool{AskConfirm: true, AskReplaceKey: replace}}
		l.calls = nil
		if err := (&Setup{Host: l.host(), UI: ui}).setUpAPI(t.Context(), m, ServiceActive); err != nil {
			t.Fatalf("setUpAPI = %v; printed %q", err, ui.printed)
		}
		var err error
		if m, _, err = LoadManifest(l.host()); err != nil {
			t.Fatal(err)
		}
		return ui
	}
	if ui := setUp(first, false); slices.Contains(ui.asked, AskReplaceKey) {
		t.Fatalf("a first key asked to replace one: %v", ui.asked)
	}

	ui := setUp(first, false)
	said(t, ui, "An API key already exists in /etc/shard/api-key.", "  Keep the API key in /etc/shard/api-key.")
	if slices.Contains(ui.printed, "  Replace the existing HTTP API service in "+serveUnit+".") {
		t.Fatalf("setup's own service reads as one installed by hand: %q", ui.printed)
	}
	if ui.initials[0] != first || !slices.Contains(ui.asked, AskReplaceKey) || called(l.calls, mintLine) >= 0 || l.read("/etc/shard/api-key") != setupKey+"\n" {
		t.Fatalf("a second run did not keep the key: asked %v, initial %q, calls %q", ui.asked, ui.initials, l.calls)
	}
	if called(l.calls, "chgrp shard "+filepath.Join(l.root, signingKeyFile)) < 0 {
		t.Fatalf("calls = %q, want the kept signing key given back to the shard group", l.calls)
	}

	l.answer[mintLine] = minted("sk_test_new")
	ui = setUp(apiServer(t, "sk_test_new"), true)
	said(t, ui, "  Replace the API key in /etc/shard/api-key. Clients that use the old key lose access.")
	revoke, mint := slices.Index(l.calls, mintLine[:len(mintLine)-len("mint")]+"revoke --name shard-setup --signing-key-file /etc/shard/serve.secret"), called(l.calls, mintLine)
	if revoke < 0 || mint < revoke || l.read("/etc/shard/api-key") != "sk_test_new\n" {
		t.Fatalf("calls = %q, want the old key revoked before a new one is saved", l.calls)
	}
	hidesTheKey(t, l, ui, "sk_test_new")
}

func TestTheHTTPAPIRowNamesAServiceInstalledByHand(t *testing.T) {
	l := newAPIHost(t)
	l.withAccount()
	m := installedWithoutAPI(t, l)
	l.write(serveUnit, "[Service]\nExecStart=/usr/local/bin/shard serve\n")
	ui := &fakeUI{texts: map[Question]string{AskListen: apiServer(t, setupKey)}, confirms: map[Question]bool{AskConfirm: true}}

	if err := (&Setup{Host: l.host(), UI: ui}).setUpAPI(t.Context(), m, ServiceActive); err != nil {
		t.Fatalf("setUpAPI = %v; printed %q", err, ui.printed)
	}
	said(t, ui, "  Replace the existing HTTP API service in "+serveUnit+".")
}

func TestTheHTTPAPIRowNeedsAStartAtBootInstallation(t *testing.T) {
	f := newFakeHost(t)
	m := linuxInstall("v0.1.0")
	m.StartAtBoot = false
	f.installed(t, m)
	ui := &fakeUI{selects: map[Question]string{AskExisting: "exit"}}

	if err := (&Setup{Host: f.host(nil), UI: ui}).existing(t.Context(), Installation{Manifest: &m, Service: ServiceNone}); err != nil {
		t.Fatalf("existing: %v", err)
	}
	i := slices.IndexFunc(ui.options[AskExisting], func(o term.Option) bool { return o.Name == ExistingHTTPAPI })
	if i < 0 || !slices.Equal(ui.options[AskExisting][i].Unavailable, []string{"The HTTP API runs as a background service, and this installation does not start shard automatically."}) {
		t.Fatalf("the menu is %+v, want the HTTP API row unavailable", ui.options[AskExisting])
	}
}

func TestRepairKeepsTheAPIKey(t *testing.T) {
	l := newAPIHost(t)
	l.withAccount()
	address := apiServer(t, setupKey)
	h := l.host()
	if err := (&Setup{Host: h, UI: apiUI(address)}).local(t.Context(), stay); err != nil {
		t.Fatalf("local = %v", err)
	}
	m, _, err := LoadManifest(h)
	if err != nil {
		t.Fatal(err)
	}
	l.calls = nil
	l.answer["systemctl is-active shard-serve.service"] = "inactive\n"
	ui := newLocalUI("true", true)

	if err := (&Setup{Host: h, UI: ui}).repair(t.Context(), m, ServiceActive, ""); err != nil {
		t.Fatalf("repair = %v; printed %q", err, ui.printed)
	}
	said(t, ui.fakeUI, "  The HTTP API service is not running.", "  Start the HTTP API", "  "+verifyAPITitle)
	if called(l.calls, mintLine) >= 0 || slices.Contains(ui.lists[0].steps, "Create the API key") || l.read("/etc/shard/api-key") != setupKey+"\n" {
		t.Fatalf("repair replaced a working key: %q", l.calls)
	}
}

func TestUpgradeRestartsTheHTTPAPI(t *testing.T) {
	rs := newReleaseServer(t)
	rs.add("v0.2.0", false, false, true, map[string]string{"shard-linux-amd64": "#!/bin/sh\necho client v0.2.0\n", "shard-init-linux-amd64": "new init"})
	f := newFakeHost(t)
	m := linuxInstall("v0.1.0")
	m.API = apiServer(t, setupKey)
	m.Files = append(m.Files, Owned{Path: serveUnit, Kind: KindService})
	f.installed(t, m)
	f.write(t, "/etc/shard/api-key", setupKey+"\n")
	ui := confirming(true)

	if err := (&Setup{Host: f.host(rs), UI: ui}).upgrade(t.Context(), m, ServiceActive, ""); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	steps := ui.lists[len(ui.lists)-1].steps
	if want := []string{"Restart the daemon", verifyDaemonTitle, "Restart the HTTP API", verifyAPITitle}; !slices.Equal(steps[len(steps)-len(want):], want) {
		t.Fatalf("the steps are %q, want them to end %q", steps, want)
	}
	if daemon, api := slices.Index(f.calls, "systemctl restart shard"), slices.Index(f.calls, "systemctl restart shard-serve.service"); daemon < 0 || api < daemon {
		t.Fatalf("calls = %q, want the HTTP API restarted after the daemon", f.calls)
	}
}

func TestUninstallStopsTheHTTPAPIBeforeTheDaemon(t *testing.T) {
	for _, c := range []struct {
		os, first, second string
		files             []Owned
		said              []string
	}{
		{"linux", "systemctl disable --now shard-serve", "systemctl disable --now shard", []Owned{{Path: serveUnit, Kind: KindService}, {Path: systemdUnit, Kind: KindService}},
			[]string{"The HTTP API keys remain in /etc/shard.", "Remove them with: sudo rm -r /etc/shard", "Remove it with: sudo userdel shard"}},
		{"darwin", "launchctl bootout system/shard.serve", "launchctl bootout system/shard.daemon", []Owned{{Path: launchdPlist, Kind: KindService}, {Path: servePlist, Kind: KindService}}, nil},
	} {
		t.Run(c.os, func(t *testing.T) {
			f := newFakeHost(t)
			m := Manifest{Version: "v0.1.0", Provider: "gvisor", StartAtBoot: true, API: serve.DefaultListen, Files: c.files}
			f.installed(t, m)
			f.launchd = "state = running\n"
			h := f.host(nil)
			h.OS = c.os
			ui := &watchUI{fakeUI: confirming(true)}

			if err := (&Setup{Host: h, UI: ui}).uninstall(t.Context(), m); err != nil {
				t.Fatalf("uninstall: %v", err)
			}
			first, second := slices.Index(f.calls, c.first), slices.Index(f.calls, c.second)
			if first < 0 || second < first {
				t.Fatalf("calls = %q, want %q before %q", f.calls, c.first, c.second)
			}
			said(t, ui.fakeUI, c.said...)
		})
	}
}

func TestVerifyAPIStops(t *testing.T) {
	gone := func(t *testing.T) string {
		t.Helper()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := ln.Addr().String()
		if err := ln.Close(); err != nil {
			t.Fatal(err)
		}
		return address
	}
	for _, c := range []struct {
		name, key, state string
		address          func(*testing.T) string
		wait             time.Duration
		want             []string
	}{
		{"on a refused key", "sk_test_wrong", "active", func(t *testing.T) string { return apiServer(t, setupKey) }, time.Hour,
			[]string{"The HTTP API refused the key in /etc/shard/api-key.", "Run shard setup again and replace the API key."}},
		{"when systemd gives up", setupKey, "failed", gone, time.Hour,
			[]string{"The HTTP API failed to start, and systemd stopped restarting it.", "Read its log with: sudo journalctl -u shard-serve.service"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			swap(t, &verifyWait, c.wait)
			swap(t, &verifyPoll, time.Millisecond)
			l := newAPIHost(t)
			l.write("/etc/shard/api-key", c.key+"\n")
			l.answer["systemctl is-active shard-serve.service"] = c.state + "\n"

			err := verifyAPI(t.Context(), l.host(), c.address(t))
			if p, ok := errors.AsType[*Problem](err); !ok || !slices.Equal(p.Lines, c.want) {
				t.Fatalf("verify = %v, want the lines %q", err, c.want)
			}
		})
	}

	t.Run("after the wait", func(t *testing.T) {
		swap(t, &verifyWait, 20*time.Millisecond)
		swap(t, &verifyPoll, time.Millisecond)
		l := newAPIHost(t)
		l.write("/etc/shard/api-key", setupKey+"\n")
		l.answer["systemctl is-active shard-serve.service"] = "activating\n"

		err := verifyAPI(t.Context(), l.host(), gone(t))
		if p, ok := errors.AsType[*Problem](err); !ok || !strings.HasPrefix(p.Lines[0], "The HTTP API did not answer after 20ms: ") {
			t.Fatalf("verify = %v, want the wait named", err)
		}
	})
}

func TestVerifyAPIReadsTheKeyPastASudoWarning(t *testing.T) {
	l := newAPIHost(t)
	h := l.host()
	h.Euid = 1000
	l.answer["sudo -n -- cat "+filepath.Join(l.root, "/etc/shard/api-key")] = "sudo: unable to resolve host box: Name or service not known\n" + setupKey + "\n"

	if err := verifyAPI(t.Context(), h, apiServer(t, setupKey)); err != nil {
		t.Fatalf("verify = %v", err)
	}
}

func TestAStartFailureNamesTheStep(t *testing.T) {
	l := newAPIHost(t)
	l.fail["systemctl restart shard-serve.service"] = "Job for shard-serve.service failed.\n"
	ui := apiUI(apiServer(t, setupKey))

	err := (&Setup{Host: l.host(), UI: ui}).local(t.Context(), stay)
	if stopped, ok := errors.AsType[*StoppedError](err); !ok || stopped.Step != "Start the HTTP API" {
		t.Fatalf("local = %v, want a stop at the HTTP API start", err)
	}
	said(t, ui.fakeUI, "Setup stopped. Earlier completed steps remain in place.")
	marks := ui.lists[1].marks
	if last := marks[len(marks)-1]; !strings.Contains(last, "Could not start the HTTP API") || !strings.Contains(last, "Job for shard-serve.service failed.") {
		t.Fatalf("last mark %q, want the failed start", last)
	}
}

func TestTheHTTPAPIOnAMacRunsAsThePerson(t *testing.T) {
	l := newLocalHost(t)
	h := l.mac()
	swap(t, &kernelURL, func(string) (string, error) { return l.rs.URL + "/download/v0.1.0/shard-init-linux-amd64", nil })
	l.answer[mintLine] = minted(setupKey)
	// shard tokens mint makes this directory, with the signing key in it, on a real Mac.
	l.mkdir(DataDir + "/auth")
	address := apiServer(t, setupKey)
	ui := newLocalUI("true", true, VZ)
	ui.selects[AskHTTPAPI] = "true"
	ui.texts = map[Question]string{AskListen: address}

	if err := (&Setup{Host: h, UI: ui}).local(t.Context(), stay); err != nil {
		t.Fatalf("local = %v; printed %q", err, ui.printed)
	}

	plist := l.read(servePlist)
	if !strings.Contains(plist, "<string>u</string>") || !strings.Contains(plist, "<string>"+address+"</string>") {
		t.Fatalf("the plist names no user or address:\n%s", plist)
	}
	if l.read("/var/lib/shard/auth/api-key") != setupKey+"\n" {
		t.Fatal("the key is not under the daemon's root")
	}
	if !slices.Contains(l.calls, mintLine+" --name shard-setup") || called(l.calls, "sudo -n -- launchctl bootstrap system "+filepath.Join(l.root, servePlist)) < 0 || called(l.calls, "getent") >= 0 {
		t.Fatalf("calls = %q, want a mint as the person and a bootstrap", l.calls)
	}
	said(t, ui.fakeUI, "  Configure and start a launchd service for the HTTP API on http://"+address+".", "  Read the key with: cat /var/lib/shard/auth/api-key")
	if slices.ContainsFunc(ui.printed, func(p string) bool { return strings.Contains(p, "sudo") }) {
		t.Fatalf("a Mac is told to use sudo: %q", ui.printed)
	}
	hidesTheKey(t, l, ui.fakeUI, setupKey)
}

func TestAPIDoneNamesTheWayIn(t *testing.T) {
	h := Host{OS: "linux", Env: func(string) string { return "" }}
	loopback := strings.Join(apiDone(h, Local{API: "127.0.0.1:7850"}), "\n")
	for _, want := range []string{"    ssh -N -L 7850:127.0.0.1:7850 <user>@<this machine>", "  Or put an HTTPS proxy in front of the HTTP API: " + remoteGuide, "  Read the key with: cat /etc/shard/api-key"} {
		if !strings.Contains(loopback, want) {
			t.Fatalf("the loopback text lacks %q:\n%s", want, loopback)
		}
	}
	vpn := strings.Join(apiDone(h, Local{API: "100.64.0.5:7850"}), "\n")
	if !strings.Contains(vpn, "  Connect a client in the same private network:\n    export SHARD_API_KEY=<the key in /etc/shard/api-key>\n    shard setup --remote http://100.64.0.5:7850 --save -y") || strings.Contains(vpn, "ssh") {
		t.Fatalf("the VPN text is:\n%s", vpn)
	}
}
