# Set up Shard

`shard setup` prepares this machine to run sandboxes, or connects the CLI to a remote Shard server.
Run it with no options and it asks each question in turn. Every question has an option, so a script
takes the same path a person does, through the same checks.

```sh
shard setup
```

## Supported platforms

| host | providers | background service |
| --- | --- | --- |
| Linux x86-64 | `firecracker` (needs `/dev/kvm`), `gvisor`, `sysbox`, `runc` | systemd |
| macOS 14 or later on Apple silicon | `vz` | launchd |

The wizard always lists all five providers. One this host cannot run is shown as unavailable, with
the reason, and cannot be chosen. A runtime setup can install does not make a provider unavailable.
Any other host, such as an Intel Mac, can still connect to a remote server.

## Local setup

The wizard asks for a provider and whether the daemon starts at boot, then checks the host, shows
what it will change, and asks once before it changes anything.

- **Provider.** On Linux it recommends Firecracker when `/dev/kvm` is usable and gVisor otherwise.
  On a supported Mac it recommends macOS Virtualization. A provider that fails setup is never
  swapped for another.
- **Automatic startup.** Yes installs and starts a systemd service on Linux or a launchd service on
  macOS, which starts the daemon at boot and restarts it after a crash. No installs the provider's
  tools and prints the `shard daemon` command to run yourself.
- **Data.** The daemon keeps its state in `/var/lib/shard`. Setup never asks for another directory.
- **Checks.** Setup checks the operating system, the provider's requirements, administrator access,
  the install paths, disk space and filesystem, download access, an earlier installation and the
  service manager before it changes the host. A failed check changes nothing.
- **Network.** Setup never exposes the HTTP API. `shard serve` stays a separate step; see
  [the daemon guide](daemon.md).

Setup runs as you and uses `sudo` for the steps that need root. It installs the running binary as
`/usr/local/bin/shard`, owned by root, and `shard-init` from the same release, after it checks that
download against the release's `SHA256SUMS`. Setup creates no `shard` group, so on Linux the API
socket belongs to root and local commands run with `sudo`. [The API socket](daemon.md#the-api-socket)
says what a group you create yourself changes.

Declining at the confirmation leaves the host as it was.

### Progress and failures

A live checklist shows each step: a gray circle for a pending step, a blue spinner for the one that
runs, a green check for a step done, yellow for one that needs attention and a red cross for a
failure. Each mark is a symbol as well as a color, and `NO_COLOR` turns the color off. When the
output is not a terminal, setup prints each step once, as it ends.

On a failure setup marks the step, says why, and stops:

```text
✗ Install gVisor
  Could not download runsc: connection timed out.

Setup stopped. Earlier completed steps remain in place.
Run `shard setup` again to retry.
```

Every step is safe to run again, so a second `shard setup` picks up where the first stopped.

## An existing installation

On a host it set up before, `shard setup` shows the version, the provider and the service, and
offers to check or repair the installation, upgrade Shard, uninstall it, or exit.

- **Check or repair** inspects the tools, the provider's files in `/var/lib/shard`, the permissions
  and the service, and shows each change before it makes one. It keeps the settings and the data.
- **Upgrade** downloads and verifies the new release before it replaces anything, keeps the old
  binary until the new one checks out, and says before it restarts the daemon.
- **Uninstall** stops and removes the service and the files setup installed. It refuses while
  sandboxes exist and names the commands that remove them. The data in `/var/lib/shard` and any
  tool another program may share stay.

Setup records what it created in `/var/lib/shard-setup/manifest.json`, and removes only what that
file lists. An installation setup did not make is reported as a manual installation; setup then
explains what it found and changes none of it.

## Remote setup

The wizard asks for the server URL, the URL of `shard serve` or the proxy in front of it. `https`
is recommended. An `http` URL works only after a warning that it encrypts neither the API key nor
the requests.

It reads the API key from `SHARD_API_KEY` when that is set, and otherwise asks for it with hidden
input. The key never appears on the screen, in a log or in an error. Then it checks that it can
reach the server, that the key is accepted, and which lifecycle verbs the server supports. A failed
check offers to retry, to edit the details, or to exit.

Certificates are always verified. For a server whose certificate a private CA signed, set
`SHARD_CA_FILE` to that CA's certificate. There is no option to skip verification.

### The saved connection

After a successful check, setup offers to save the connection so every later command uses it. The
file is `$XDG_CONFIG_HOME/shard/config.json`, or `~/.config/shard/config.json` when that is unset:

```json
{
  "remote": "https://shard.example.com",
  "api_key": "<saved key>"
}
```

The key is stored as plain text, so setup makes the directory and the file readable by your user
only, and replaces the file whole. A command resolves its server and key in this order:

| setting | first | then | last |
| --- | --- | --- | --- |
| server URL | `--remote` | `SHARD_REMOTE` | the saved connection |
| API key | `SHARD_API_KEY` | | the saved connection |

A verb that runs on the daemon host only refuses a saved connection, and `--remote ""` runs it on
this host. Run `shard setup` again to check, replace or remove the saved connection. Choosing local
setup while a connection is saved offers to remove it, and removes it only once local setup
succeeds; a repair or an upgrade of an existing installation makes the same offer. A `SHARD_REMOTE`
in the environment still overrides the local daemon after that.

## Automated setup

| option | what |
| --- | --- |
| `--local` | set up this machine to run sandboxes |
| `--remote <url>` | connect to a remote Shard server |
| `--provider <name>` | `firecracker`, `gvisor`, `sysbox`, `runc` or `vz` |
| `--start-at-boot <true\|false>` | install the background service, or leave the daemon to you |
| `--save` | save the remote connection |
| `-y`, `--yes` | apply the changes without the confirmation |

```sh
shard setup --local --provider gvisor --start-at-boot=true -y
SHARD_API_KEY=... shard setup --remote https://shard.example.com --save -y
```

An option answers its question and the wizard asks the rest. Without a terminal, every question
must have its answer: setup fails and names the option it needs, and it never picks one for you.
`-y` answers the confirmation and the `http` warning only. It skips no check, and it saves a
connection only with `--save`. The API key comes from `SHARD_API_KEY`, never from an option. Setup
refuses a local option beside `--remote`, and `--save` beside a local option.

## Exit codes

| code | when |
| --- | --- |
| 0 | setup finished, or there was nothing to do |
| 1 | a check or a step failed, an option was refused, or the confirmation was declined |
| 130 | Ctrl+C left setup; the steps done so far stay in place |
