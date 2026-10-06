package setup

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/presmihaylov/shard/pkg/term"
	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/serve"
)

// The files and names of the HTTP API, which shard serve answers beside the daemon.
const (
	serveUnit      = "/etc/systemd/system/shard-serve.service"
	serveUnitName  = "shard-serve.service"
	servePlist     = "/Library/LaunchDaemons/shard.serve.plist"
	serveLabel     = "system/shard.serve"
	serveLog       = macLogDir + "/serve.log"
	apiDir         = "/etc/shard"
	signingKeyFile = apiDir + "/serve.secret"
	// apiAccount runs shard serve on Linux; the daemon gives its group the API socket.
	apiAccount = "shard"
	// apiKeyName is the subject of every key setup mints, so a replacement revokes them all.
	apiKeyName = "shard-setup"
	// lowestPort is the first port an account without root may bind.
	lowestPort     = 1024
	apiCheckTitle  = "HTTP API address and service"
	verifyAPITitle = "Verify an authenticated API request"
	remoteGuide    = "https://useshards.com/docs/guides/remote/#3-put-https-in-front"
)

// apiKeyFile holds the key setup minted: root's alone on Linux, and the person's under the daemon's root on a Mac, where shard serve runs as them.
func apiKeyFile(h Host) string {
	if h.OS == "darwin" {
		return path.Join(DataDir, serve.AuthDir, "api-key")
	}

	return apiDir + "/api-key"
}

// CheckListen refuses an address the HTTP API must not listen on, before setup changes anything.
func CheckListen(address string) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%q is not an IP address and port, such as %s", address, serve.DefaultListen)
	}
	if ap.Addr().Zone() != "" {
		return fmt.Errorf("%s names an IPv6 zone, which a client URL cannot carry; name an address without one", address)
	}
	if ap.Addr().IsUnspecified() {
		return fmt.Errorf("%s listens on every network, and the HTTP API serves plain HTTP; name one address, such as %s or a VPN address", address, serve.DefaultListen)
	}
	if ap.Port() < lowestPort {
		return fmt.Errorf("port %d needs root, and shard serve runs without it; choose a port from %d up", ap.Port(), lowestPort)
	}

	return nil
}

// askAPI is the HTTP API question after automatic startup, and the address on a yes; "" is local connections only.
func (s *Setup) askAPI(ctx context.Context) (string, error) {
	options := []term.Option{
		{Name: "true", Label: "Yes - Configure the API as a background service"},
		{Name: "false", Label: "No  - Use local connections only", Default: true},
	}
	i, err := s.UI.Select(ctx, AskHTTPAPI, "Set up the HTTP API?\n\nConnect through an HTTPS proxy, an SSH tunnel, or a private network.", options)
	if err != nil || options[i].Name != "true" {
		return "", err
	}

	return s.askListen(ctx, serve.DefaultListen)
}

func (s *Setup) askListen(ctx context.Context, initial string) (string, error) {
	prompt := "Listen address for the HTTP API:\n" +
		"  " + serve.DefaultListen + " accepts connections only from this machine.\n" +
		"  A VPN address permits direct access through that private network."
	for {
		address, err := s.UI.Text(ctx, AskListen, prompt, initial)
		if err != nil {
			return "", err
		}
		address = strings.TrimSpace(address)
		err = CheckListen(address)
		if err == nil {
			return address, nil
		}
		if err := s.UI.Print(sentence(err), ""); err != nil {
			return "", err
		}
	}
}

func apiServiceFile(h Host) string {
	if h.OS == "darwin" {
		return servePlist
	}

	return serveUnit
}

// signingKeyPath is the key that signs every API key: setup's own on Linux, and the default under the daemon's root on a Mac.
func signingKeyPath(h Host) string {
	if h.OS == "darwin" {
		return path.Join(DataDir, serve.AuthDir, serve.SigningKeyFileName)
	}

	return signingKeyFile
}

func (s *Setup) keys(ctx context.Context) (key, signing bool, err error) {
	// /etc/shard keeps everyone but root and the shard group out, so only sudo can look.
	if _, err := os.Lstat(rooted(s.Host, apiKeyFile(s.Host))); errors.Is(err, fs.ErrPermission) {
		if err := s.admin(ctx); err != nil {
			return false, false, err
		}
	}
	if key, err = present(ctx, s.Host, apiKeyFile(s.Host)); err != nil {
		return false, false, err
	}
	signing, err = present(ctx, s.Host, signingKeyPath(s.Host))

	return key, signing, err
}

// keyChoice finds a key an earlier setup left, and keeps it unless the person chooses to replace it; a key whose signing key is gone no longer works, so setup makes a new one.
func (s *Setup) keyChoice(ctx context.Context, l Local) (Local, error) {
	key, signing, err := s.keys(ctx)
	if err != nil || !key || !signing {
		return l, err
	}
	file := apiKeyFile(s.Host)
	l.keyFound = true
	if err := s.UI.Print("An API key already exists in "+file+".", "A new key ends access for every client that uses the old one.", ""); err != nil {
		return l, err
	}
	l.ReplaceKey, err = s.UI.Confirm(ctx, AskReplaceKey, "Replace the API key?", false)

	return l, err
}

// present says whether file exists, through sudo where a root-only directory keeps setup's user from looking.
func present(ctx context.Context, h Host, file string) (bool, error) {
	_, err := os.Lstat(rooted(h, file))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case !errors.Is(err, fs.ErrPermission):
		return false, fmt.Errorf("check %s: %w", file, err)
	}
	out, err := privileged(ctx, h, "ls", "-A", rooted(h, path.Dir(file)))
	if err != nil {
		return false, fmt.Errorf("check %s: %w", file, err)
	}

	return slices.Contains(strings.Split(string(out), "\n"), path.Base(file)), nil
}

// apiProblems is what repair finds wrong with a recorded HTTP API, and it keeps a key that still works; account says the shard account is gone.
func (s *Setup) apiProblems(ctx context.Context, l *Local) (problems []string, account bool, err error) {
	h := s.Host
	key, signing, err := s.keys(ctx)
	if err != nil {
		return nil, false, err
	}
	l.keyFound = key && signing
	if !key {
		problems = append(problems, "The API key file "+apiKeyFile(h)+" is missing.")
	}
	if !signing {
		problems = append(problems, "The signing key "+signingKeyPath(h)+" is missing, so setup creates a new API key.")
	}
	if h.OS == "linux" {
		group, user, err := accountMissing(ctx, h)
		if err != nil {
			return nil, false, err
		}
		if account = group || user; account {
			problems = append(problems, "The "+apiAccount+" account, which runs the HTTP API, is missing.")
		}
	}
	state, err := apiState(ctx, h)
	if err != nil {
		return nil, false, err
	}
	if state != ServiceActive {
		problems = append(problems, "The HTTP API service is not running.")
	}

	return problems, account, nil
}

// setUpAPI is the §11 row: it adds the HTTP API to an installation, or changes its address or key, so a No at first setup needs no uninstall.
func (s *Setup) setUpAPI(ctx context.Context, m Manifest, service ServiceState) error {
	initial := m.API
	if initial == "" {
		initial = serve.DefaultListen
	}
	address, err := s.askListen(ctx, initial)
	if err != nil {
		return err
	}
	l := Local{Provider: m.Provider, StartAtBoot: m.StartAtBoot, StorageMiB: m.StorageMiB, API: address}
	f, err := s.runChecks(ctx, []check{{"Administrator access", administratorAccess}, {apiCheckTitle, apiCheck}}, l)
	if err != nil {
		return err
	}
	if f != nil {
		return errors.Join(s.UI.Print("", "No installation changes were made."), &StoppedError{Step: f.check, Err: &Problem{Lines: f.lines}})
	}
	if l, err = s.keyChoice(ctx, l); err != nil {
		return err
	}
	account, missing, err := accountStep(ctx, s.Host)
	if err != nil {
		return err
	}
	will, notes := apiReview(s.Host, l, missing)
	file := apiServiceFile(s.Host)
	exists, err := present(ctx, s.Host, file)
	if err != nil {
		return err
	}
	// Uninstall removes the file once setup has written it, so a person who installed it by hand learns it is replaced.
	if exists && !slices.ContainsFunc(m.Files, func(f Owned) bool { return f.Path == file }) {
		will = append(will, "  Replace the existing HTTP API service in "+file+".")
	}
	var steps []Step
	if missing {
		steps = append(steps, account)
	}
	daemon := newLocalPlan(s.Host, l)
	switch {
	// The daemon gives the new shard group its socket only at a start.
	case missing && service == ServiceActive:
		will = append(will, "  Restart the daemon, so the HTTP API can reach its socket. Your sandboxes keep running.")
		steps = append(steps, Step{Title: "Restart the daemon", Do: func(ctx context.Context) error { return restartService(ctx, s.Host, m) }})
	case service != ServiceActive:
		will = append(will, "  Start the daemon, which the HTTP API needs.")
		steps = append(steps, Step{Title: "Start the daemon", Do: daemon.start})
	}
	if missing || service != ServiceActive {
		steps = append(steps, Step{Title: verifyDaemonTitle, Do: func(ctx context.Context) error { return verifyDaemon(ctx, s.Host) }})
	}
	steps = append(steps, apiSteps(s.Host, l)...)

	lines := slices.Concat([]string{"", "Ready to set up the HTTP API", "", "Setup will:"}, will, []string{""}, notes, []string{"", "Administrator access is required.", ""})
	if err := s.UI.Print(lines...); err != nil {
		return err
	}
	if err := s.confirm(ctx, true); err != nil {
		return err
	}
	if err := s.admin(ctx); err != nil {
		return err
	}
	if err := s.apply(ctx, "Setting up the HTTP API", steps); err != nil {
		return err
	}

	return s.UI.Print(append([]string{"", "The HTTP API is set up.", ""}, apiDone(s.Host, l)...)...)
}

// apiCheck is the HTTP API's preflight: the account can be made, the key directory is root's, and the address is free.
func apiCheck(ctx context.Context, h Host, l Local) *finding {
	if h.OS == "linux" {
		if f := accountTools(ctx, h); f != nil {
			return f
		}
		if f := rootOnly(h, apiDir, "it holds the keys of the HTTP API"); f != nil {
			return f
		}
	}

	return addressFree(ctx, h, l.API)
}

func accountTools(ctx context.Context, h Host) *finding {
	group, user, err := accountMissing(ctx, h)
	if err != nil {
		return failed(sentence(err))
	}
	for _, t := range []struct {
		missing bool
		tool    string
	}{{group, "groupadd"}, {user, "useradd"}} {
		if _, ok := lookPath(h, t.tool); t.missing && !ok {
			return failed(fmt.Sprintf("The HTTP API runs as the %s account, and this machine has no %s to create it.", apiAccount, t.tool))
		}
	}

	return nil
}

// addressFree binds the address once, so a taken port stops setup before any change; the HTTP API setup already runs there holds it.
func addressFree(ctx context.Context, h Host, address string) *finding {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return failed(sentence(err))
	}
	m, ok, err := LoadManifest(h)
	if err != nil {
		return failed(sentence(err))
	}
	if ok && m.API == address {
		state, err := apiState(ctx, h)
		if err != nil {
			return failed(sentence(err))
		}
		if state == ServiceActive {
			return nil
		}
	}

	ln, err := h.Listen("tcp", address)
	switch {
	case err == nil:
		if err := ln.Close(); err != nil {
			return failed(sentence(err))
		}
		return nil
	case errors.Is(err, syscall.EADDRINUSE):
		return failed(fmt.Sprintf("Port %d is in use by another program.", ap.Port()), "Stop that program, or run shard setup again and choose another port.")
	case errors.Is(err, syscall.EADDRNOTAVAIL):
		return failed(ap.Addr().String()+" is not an address of this machine.", "Run shard setup again and choose an address this machine has, such as 127.0.0.1 or its VPN address.")
	}

	return failed(fmt.Sprintf("Could not listen on %s: %v.", address, err))
}

// accountMissing says which half of the shard account this host lacks: the group, the user, or both.
func accountMissing(ctx context.Context, h Host) (group, user bool, err error) {
	if group, err = lacksEntry(ctx, h, "group"); err != nil {
		return false, false, err
	}
	user, err = lacksEntry(ctx, h, "passwd")

	return group, user, err
}

func lacksEntry(ctx context.Context, h Host, db string) (bool, error) {
	out, err := h.Run(ctx, "getent", db, apiAccount)
	if err == nil {
		return strings.TrimSpace(string(out)) == "", nil
	}
	// getent exits 2 when the database has no such key.
	if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() == 2 {
		return true, nil
	}

	return false, fmt.Errorf("look up the %s account in %s: %w", apiAccount, db, err)
}

// accountStep makes the shard account when this Linux host lacks it, and reports whether it will.
func accountStep(ctx context.Context, h Host) (Step, bool, error) {
	if h.OS != "linux" {
		return Step{}, false, nil
	}
	group, user, err := accountMissing(ctx, h)
	if err != nil || !group && !user {
		return Step{}, false, err
	}

	return Step{Title: "Create the shard account", Do: func(ctx context.Context) error { return createAccount(ctx, h) }}, true, nil
}

// createAccount looks again, so a retry makes only what an earlier run did not.
func createAccount(ctx context.Context, h Host) error {
	group, user, err := accountMissing(ctx, h)
	if err != nil {
		return err
	}
	if group {
		if _, err := privileged(ctx, h, "groupadd", "--system", apiAccount); err != nil {
			return &Problem{Lines: []string{fmt.Sprintf("Could not create the %s group: %v.", apiAccount, err)}}
		}
	}
	if user {
		if _, err := privileged(ctx, h, "useradd", "--system", "--no-create-home", "--shell", "/usr/sbin/nologin", "--gid", apiAccount, apiAccount); err != nil {
			return &Problem{Lines: []string{fmt.Sprintf("Could not create the %s user: %v.", apiAccount, err)}}
		}
	}

	return nil
}

// apiPlan is the HTTP API part of one setup, which runs after the daemon is up.
type apiPlan struct {
	h     Host
	local Local
	// user runs shard serve on a Mac and owns the key file there.
	user string
}

// apiSteps configure, key, start and verify the HTTP API, in checklist order; each is safe to run again.
func apiSteps(h Host, l Local) []Step {
	p := &apiPlan{h: h, local: l, user: daemonUser(h)}
	steps := []Step{{Title: "Configure the HTTP API service", Do: p.service}}
	switch {
	case !l.keyFound:
		steps = append(steps, Step{Title: "Create the API key", Do: p.createKey})
	case l.ReplaceKey:
		steps = append(steps, Step{Title: "Replace the API key", Do: p.replaceKey})
	}

	return append(steps,
		Step{Title: "Start the HTTP API", Do: p.start},
		Step{Title: verifyAPITitle, Do: p.verify},
	)
}

func (p *apiPlan) service(ctx context.Context) (err error) {
	dir, err := os.MkdirTemp("", "shard-setup-")
	if err != nil {
		return fmt.Errorf("create a staging directory: %w", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			err = errors.Join(err, fmt.Errorf("remove the staging directory: %w", rmErr))
		}
	}()

	if p.h.OS == "darwin" {
		return p.launchdService(ctx, dir)
	}

	return p.systemdService(ctx, dir)
}

func (p *apiPlan) systemdService(ctx context.Context, dir string) error {
	// setgid keeps every file in it in the shard group, which shard serve reads them as.
	if _, err := privileged(ctx, p.h, "install", "-d", "-o", "0", "-g", apiAccount, "-m", "2750", rooted(p.h, apiDir)); err != nil {
		return &Problem{Lines: []string{fmt.Sprintf("Could not create %s: %v.", apiDir, err)}}
	}
	if err := p.regroup(ctx); err != nil {
		return err
	}
	if err := p.signingKey(ctx, dir); err != nil {
		return err
	}
	unit := filepath.Join(dir, serveUnitName)
	if err := os.WriteFile(unit, []byte(serveUnitText(p.local.API)), 0o600); err != nil {
		return err
	}
	if _, err := run(ctx, p.h, "systemd-analyze", "verify", unit); err != nil {
		return &Problem{Lines: []string{fmt.Sprintf("systemd refused the HTTP API service definition: %v.", err)}}
	}
	if err := installFile(ctx, p.h, unit, serveUnit, "0644"); err != nil {
		return &Problem{Lines: []string{fmt.Sprintf("Could not install %s: %v.", serveUnit, err)}}
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", serveUnitName}} {
		if _, err := privileged(ctx, p.h, "systemctl", args...); err != nil {
			return &Problem{Lines: []string{fmt.Sprintf("Could not enable the HTTP API service: %v.", err)}}
		}
	}

	return p.record(ctx, serveUnit)
}

// regroup gives the files shard serve reads back to the shard group, since an account made again after an uninstall can get another id.
func (p *apiPlan) regroup(ctx context.Context) error {
	for _, file := range []string{signingKeyFile, serve.TokensPath(signingKeyFile)} {
		found, err := present(ctx, p.h, file)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		if _, err := privileged(ctx, p.h, "chgrp", apiAccount, rooted(p.h, file)); err != nil {
			return &Problem{Lines: []string{fmt.Sprintf("Could not give %s to the %s group: %v.", file, apiAccount, err)}}
		}
	}

	return nil
}

// signingKey makes the key that signs API keys, and keeps one that is there, since a new one would end every key it signed.
func (p *apiPlan) signingKey(ctx context.Context, dir string) error {
	found, err := present(ctx, p.h, signingKeyFile)
	if err != nil || found {
		return err
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Errorf("read random bytes for the signing key: %w", err)
	}
	staged := filepath.Join(dir, "serve.secret")
	if err := os.WriteFile(staged, []byte(hex.EncodeToString(raw[:])+"\n"), 0o600); err != nil {
		return err
	}
	if _, err := privileged(ctx, p.h, "install", "-o", "0", "-g", apiAccount, "-m", "0640", staged, rooted(p.h, signingKeyFile)); err != nil {
		return &Problem{Lines: []string{fmt.Sprintf("Could not install %s: %v.", signingKeyFile, err)}}
	}

	return nil
}

func (p *apiPlan) launchdService(ctx context.Context, dir string) error {
	plist := filepath.Join(dir, "shard.serve.plist")
	if err := os.WriteFile(plist, []byte(servePlistText(p.user, p.local.API)), 0o600); err != nil {
		return err
	}
	if _, err := run(ctx, p.h, "plutil", "-lint", plist); err != nil {
		return &Problem{Lines: []string{fmt.Sprintf("launchd refused the HTTP API service definition: %v.", err)}}
	}
	if err := installFile(ctx, p.h, plist, servePlist, "0644"); err != nil {
		return &Problem{Lines: []string{fmt.Sprintf("Could not install %s: %v.", servePlist, err)}}
	}

	return p.record(ctx, servePlist)
}

// record names the address and the service file in the manifest, and leaves the version alone, since the HTTP API row over an installation replaces no binary.
func (p *apiPlan) record(ctx context.Context, file string) error {
	return record(ctx, p.h, func(m *Manifest) { m.API = p.local.API }, Owned{Path: file, Kind: KindService})
}

func (p *apiPlan) createKey(ctx context.Context) error {
	key, err := p.mint(ctx)
	if err != nil {
		return err
	}

	return p.saveKey(ctx, key)
}

// replaceKey revokes every key setup minted before it saves a new one, so the old key stops working.
func (p *apiPlan) replaceKey(ctx context.Context) error {
	if _, err := p.shard(ctx, "tokens", "revoke", "--name", apiKeyName); err != nil {
		return &Problem{Lines: []string{fmt.Sprintf("Could not revoke the old API key: %v.", err)}}
	}

	return p.createKey(ctx)
}

// mint signs a key with shard tokens mint, which records it in the ledger; the key never reaches an error or the screen.
func (p *apiPlan) mint(ctx context.Context) (string, error) {
	out, err := p.shard(ctx, "tokens", "mint", "--name", apiKeyName)
	if err != nil {
		return "", &Problem{Lines: []string{fmt.Sprintf("Could not create the API key: %v.", err)}}
	}
	var minted serve.Token
	// sudo can warn on stderr before the JSON line; a decode error stays out, since it can quote a character of the key.
	if json.Unmarshal(lastLine(out), &minted) != nil || minted.Token == "" {
		return "", &Problem{Lines: []string{"Could not create the API key: shard tokens mint printed no key."}}
	}

	return minted.Token, nil
}

// shard runs a tokens verb on the ledger shard serve reads: root's under /etc/shard on Linux, and the person's under the daemon's root on a Mac.
func (p *apiPlan) shard(ctx context.Context, args ...string) ([]byte, error) {
	cmd := append([]string{shardBinary, "--root", DataDir, "--remote", ""}, args...)
	if p.h.OS == "linux" {
		return privileged(ctx, p.h, cmd[0], append(cmd[1:], "--signing-key-file", signingKeyFile)...)
	}
	if p.h.Euid == 0 {
		return run(ctx, p.h, "sudo", append([]string{"-n", "-u", p.user, "--"}, cmd...)...)
	}

	return run(ctx, p.h, cmd[0], cmd[1:]...)
}

// saveKey stages the key at 0600 and installs it with its owner, so no one else can read it at any moment.
func (p *apiPlan) saveKey(ctx context.Context, key string) (err error) {
	dir, err := os.MkdirTemp("", "shard-setup-")
	if err != nil {
		return fmt.Errorf("create a staging directory: %w", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			err = errors.Join(err, fmt.Errorf("remove the staging directory: %w", rmErr))
		}
	}()
	staged := filepath.Join(dir, "api-key")
	if err := os.WriteFile(staged, []byte(key+"\n"), 0o600); err != nil {
		return err
	}
	file, owner := apiKeyFile(p.h), "0"
	if p.h.OS == "darwin" {
		owner = p.user
	}
	if _, err := privileged(ctx, p.h, "install", "-o", owner, "-m", "0600", staged, rooted(p.h, file)); err != nil {
		return &Problem{Lines: []string{fmt.Sprintf("Could not save the API key in %s: %v.", file, err)}}
	}

	return nil
}

// start restarts a running front, since a new address or plist takes effect only on a fresh start.
func (p *apiPlan) start(ctx context.Context) error {
	if p.h.OS != "darwin" {
		if _, err := privileged(ctx, p.h, "systemctl", "restart", serveUnitName); err != nil {
			return &Problem{Lines: []string{fmt.Sprintf("Could not start the HTTP API: %v.", err)}}
		}
		return nil
	}

	if err := bootout(ctx, p.h, serveLabel); err != nil {
		return &Problem{Lines: []string{fmt.Sprintf("Could not stop the HTTP API: %v.", err)}}
	}
	if _, err := privileged(ctx, p.h, "launchctl", "bootstrap", "system", rooted(p.h, servePlist)); err != nil {
		return &Problem{Lines: []string{fmt.Sprintf("Could not start the HTTP API: %v.", err)}}
	}

	return nil
}

// verify asks the daemon for its version through the HTTP API with the saved key, which proves the key, the front and the daemon behind it.
func (p *apiPlan) verify(ctx context.Context) error {
	return verifyAPI(ctx, p.h, p.local.API)
}

func verifyAPI(ctx context.Context, h Host, address string) error {
	key, err := readKey(ctx, h)
	if err != nil {
		return err
	}
	c, err := client.NewRemote("http://"+address, key, nil)
	if err != nil {
		return &Problem{Lines: []string{fmt.Sprintf("Could not use the API key in %s: %v.", apiKeyFile(h), err)}}
	}
	deadline := time.Now().Add(verifyWait)
	for {
		_, err := c.Version(ctx)
		if err == nil {
			return nil
		}
		if refused, ok := errors.AsType[*client.APIError](err); ok && refused.Status == http.StatusUnauthorized {
			return &Problem{Lines: []string{"The HTTP API refused the key in " + apiKeyFile(h) + ".", "Run shard setup again and replace the API key."}}
		}
		if err := apiFailed(ctx, h); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return &Problem{Lines: []string{fmt.Sprintf("The HTTP API did not answer after %s: %v.", verifyWait, err), apiLogHint(h)}}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(verifyPoll):
		}
	}
}

// apiFailed ends the wait once systemd gives up on the front; launchd keeps restarting it, so a Mac waits out verifyWait.
func apiFailed(ctx context.Context, h Host) error {
	if h.OS != "linux" {
		return nil
	}
	state, err := unitState(ctx, h, serveUnitName)
	if err != nil {
		return err
	}
	if state == "failed" {
		return &Problem{Lines: []string{"The HTTP API failed to start, and systemd stopped restarting it.", apiLogHint(h)}}
	}

	return nil
}

// readKey reads the key file, through sudo where it is root's and setup is not.
func readKey(ctx context.Context, h Host) (string, error) {
	file := apiKeyFile(h)
	data, err := keyBytes(ctx, h, file)
	if err != nil {
		return "", fmt.Errorf("read the API key: %w", err)
	}
	// sudo cat can put a warning before the key.
	key := string(lastLine(data))
	if key == "" {
		return "", &Problem{Lines: []string{"The API key file " + file + " is empty.", "Run shard setup again and replace the API key."}}
	}

	return key, nil
}

func lastLine(out []byte) []byte {
	trimmed := bytes.TrimSpace(out)

	return trimmed[bytes.LastIndexByte(trimmed, '\n')+1:]
}

func keyBytes(ctx context.Context, h Host, file string) ([]byte, error) {
	if h.OS == "linux" && h.Euid != 0 {
		return privileged(ctx, h, "cat", rooted(h, file))
	}

	return os.ReadFile(rooted(h, file))
}

func apiLogHint(h Host) string {
	if h.OS == "darwin" {
		return "Read its log in " + serveLog + "."
	}

	return "Read its log with: " + sudoFor(h) + "journalctl -u " + serveUnitName
}

func apiState(ctx context.Context, h Host) (ServiceState, error) {
	if h.OS == "darwin" {
		return launchdState(ctx, h, serveLabel)
	}
	state, err := unitState(ctx, h, serveUnitName)
	if err != nil {
		return "", err
	}
	if state == "active" {
		return ServiceActive, nil
	}

	return ServiceInactive, nil
}

// restartAPI follows a daemon restart, so a Mac front, which launchd does not tie to the daemon, runs the new binary too.
func restartAPI(ctx context.Context, h Host) error {
	cmd := []string{"systemctl", "restart", serveUnitName}
	if h.OS == "darwin" {
		cmd = []string{"launchctl", "kickstart", "-k", serveLabel}
	}
	if _, err := privileged(ctx, h, cmd[0], cmd[1:]...); err != nil {
		return fmt.Errorf("restart the HTTP API: %w", err)
	}

	return nil
}

// apiReview is the HTTP API part of the review: what setup will do, and what the API does not do.
func apiReview(h Host, l Local, account bool) (will, notes []string) {
	if account {
		will = append(will, "  Create the "+apiAccount+" account, which runs the HTTP API.")
	}
	file := apiKeyFile(h)
	switch {
	case !l.keyFound:
		will = append(will, "  Create an API key in "+file+".")
	case l.ReplaceKey:
		will = append(will, "  Replace the API key in "+file+". Clients that use the old key lose access.")
	default:
		will = append(will, "  Keep the API key in "+file+".")
	}
	manager := "systemd"
	if h.OS == "darwin" {
		manager = "launchd"
	}
	will = append(will, "  Configure and start a "+manager+" service for the HTTP API on http://"+l.API+".")
	notes = []string{
		"The HTTP API serves plain HTTP. Setup does not configure HTTPS.",
		"API requests fail when the daemon is stopped.",
	}

	return will, notes
}

// apiDone is where the HTTP API answers, where its key is, and how a client reaches it: a tunnel or a proxy for a loopback address, and the address itself on a private network.
func apiDone(h Host, l Local) []string {
	file := apiKeyFile(h)
	read := "cat " + file
	if h.OS == "linux" {
		read = sudoFor(h) + read
	}
	lines := []string{
		"HTTP API:     http://" + l.API,
		"API key file: " + file,
		"  Read the key with: " + read,
		"API requests fail when the daemon is stopped.",
		"",
	}
	// CheckListen let the address through, so it parses.
	ap := netip.MustParseAddrPort(l.API)
	client := []string{
		"    export SHARD_API_KEY=<the key in " + file + ">",
		"    shard setup --remote http://" + l.API + " --save -y",
	}
	if !ap.Addr().IsLoopback() {
		return slices.Concat(lines, []string{"  Connect a client in the same private network:"}, client, []string{""})
	}
	tunnel := fmt.Sprintf("    ssh -N -L %d:%s <user>@<this machine>", ap.Port(), l.API)

	return slices.Concat(lines,
		[]string{"  Connect a client through an SSH tunnel. On the client, run:", tunnel, "  Then, in another terminal on the client:"},
		client,
		[]string{"", "  Or put an HTTPS proxy in front of the HTTP API: " + remoteGuide, ""},
	)
}

// serveUnitText is packaging/systemd/shard-serve.service with the address named.
func serveUnitText(address string) string {
	return strings.Replace(serveUnitTemplate, serveExecStart, serveExecStart+" --listen "+address, 1)
}

const serveExecStart = "ExecStart=" + shardBinary + " --root " + DataDir + " serve --signing-key-file " + signingKeyFile

// servePlistText is packaging/launchd/shard.serve.plist with the person and the address put in.
func servePlistText(user, address string) string {
	text := strings.ReplaceAll(servePlistTemplate, "__USER__", user)

	return strings.Replace(text, "<string>"+serve.DefaultListen+"</string>", "<string>"+address+"</string>", 1)
}

const serveUnitTemplate = `# shard serve is the network-facing process, off unless it is installed on purpose. It is not root:
# the daemon owns the sandboxes, and this one only carries bytes to its socket.
[Unit]
Description=shard serve, the TCP front of the shard daemon
After=shard.service network-online.target
Wants=network-online.target
BindsTo=shard.service

[Service]
Type=simple
# Plain HTTP on 127.0.0.1:7850; a proxy on this host gives clients HTTPS (https://useshards.com/docs/guides/remote/#3-put-https-in-front).
ExecStart=/usr/local/bin/shard --root /var/lib/shard serve --signing-key-file /etc/shard/serve.secret
# The unprivileged account of the front. Its only privilege is the group that may reach the socket.
User=shard
Group=shard
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true
ProtectSystem=strict
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
# Connecting to a unix socket needs write on it, which a read-only /var would refuse.
ReadWritePaths=/var/lib/shard
Restart=on-failure
RestartSec=1

[Install]
# Wanted by the daemon too, so a start of the daemon after a stop brings the front back.
WantedBy=multi-user.target shard.service
`

const servePlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- shard serve is the network-facing process, off unless it is installed on purpose. The LaunchDaemon mirror of packaging/systemd/shard-serve.service. -->
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>shard.serve</string>
	<!-- Plain HTTP; a proxy on this host gives clients HTTPS (https://useshards.com/docs/guides/remote/#3-put-https-in-front). Setup puts the address in. -->
	<key>ProgramArguments</key>
	<array>
		<string>/usr/local/bin/shard</string>
		<string>serve</string>
		<string>--listen</string>
		<string>127.0.0.1:7850</string>
	</array>
	<!-- The daemon's own user, who owns its socket and the signing key under /var/lib/shard/auth. -->
	<key>UserName</key>
	<string>__USER__</string>
	<!-- launchd has no BindsTo=: this front runs while the daemon is stopped, and every request fails until it is back. -->
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>1</integer>
	<key>StandardOutPath</key>
	<string>/var/log/shard/serve.log</string>
	<key>StandardErrorPath</key>
	<string>/var/log/shard/serve.log</string>
</dict>
</plist>
`
