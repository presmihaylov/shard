# Run shard on a Mac

On a Mac, one `shard` binary runs every verb, with one VM per sandbox. It drives
Virtualization.framework itself. The daemon boots a Linux micro VM for each sandbox, over shard's own
kernel and a disk it builds from the image, and the CLI is the same as on Linux. There is nothing to
install first, neither Docker nor Homebrew, and the daemon runs as your user.

shard needs Apple silicon and macOS 14 or later, which is the one supported Mac. An Intel Mac is not
supported. The binary builds and the framework boots there, but nothing is tested on it, `pause`,
`resume` and `fork` refuse by name on every macOS, and a bug on Intel gets no fix. macOS 13 is not
supported either, because the three snapshot verbs need the save API of macOS 14 and refuse by name
on 13 (`docs/provider-vz.md`). There is no fallback provider. The appendix is the workaround for both.

## Get the binary

Download `shard-darwin-arm64` from the latest release, put it on the path, and give it a root:

```
curl -fsSLo shard https://github.com/presmihaylov/shard/releases/latest/download/shard-darwin-arm64
chmod +x shard && sudo install -d -m0755 /usr/local/bin && sudo install -m0755 shard /usr/local/bin/shard
sudo install -d -o "$USER" /var/lib/shard
```

A Mac without the developer tools has no `/usr/local/bin`, so the first `install -d` makes it. The
root is where the daemon keeps every record, disk and kernel, and it defaults to `/var/lib/shard`.
The second `install -d` gives it to your user, so nothing runs as root. `--root <dir>` on every
command picks another root of at most 58 bytes, and needs no `sudo` at all. A binary that a browser
fetched carries the quarantine flag, and macOS refuses to run it. `xattr -d com.apple.quarantine shard`
clears the flag. `curl` does not set it.

## Run it

```
shard daemon
```

This starts the daemon in a terminal of its own, and it stays in the foreground there. On a Mac it
picks the `vz` provider by itself, and `shard info` prints that choice and the reason for it, even
before a daemon is up. A root that already holds records keeps the provider that made them.

The daemon builds the provider the first time it needs it. With records in the root, that happens
at boot, because the daemon checks each record against the substrate before it listens. With an
empty root, it happens on the first verb that needs the provider. The build writes the signed VM
shim and the guest supervisor under the root (`docs/macos-signing.md`). It also fetches the release
kernel for this Mac into the root and checks it against its hash (`docs/kernel.md`). A daemon of the
same build hashes all three and keeps them. A new build replaces the shim and the supervisor, and
fetches the kernel again when its tag moves or its bytes changed.

If the build fails over a root with records, the daemon does not start. A release endpoint that it
cannot reach holds it for up to 5 minutes. Then it exits with the error, and under launchd it starts
again and retries. Over an empty root the daemon starts, and only the verb that needs the provider
fails.

In a second terminal:

```
shard create --memory 512 python:3.12 -- python -c 'print(1)'
shard logs <id>
shard exec <id> -- uname -a
shard pause <id>
shard resume <id>
shard stop <id>
```

shard pulls the image from the registry and turns it into an ext4 disk once. Every sandbox over that
image boots an APFS clone of the disk, so the second `create` of an image costs only a boot.
`--memory` is required and is a hard cap, because a VM's memory is real memory on a laptop. `0`,
which means unbounded on Linux, is refused by name rather than mapped to a default. 128 MiB is the
smallest a sandbox boots with. Keep the number of running sandboxes within what the Mac can hold:
eight of 512 MB is 4 GB.

The VM has no route onto the LAN. Its network is a file handle into the daemon, where shard's own
netstack terminates it. The only things it can reach are the egress proxy and the resolver
(`docs/egress.md`). So a `--policy` holds on a Mac the same as on Linux, and a sandbox cannot see
your printer.

## Keep it up

Closing the terminal ends the daemon. To have launchd keep it running instead, use the LaunchDaemon
in `packaging/launchd/shard.daemon.plist`, which mirrors the Linux unit. It starts the daemon at boot
as your user. It brings the daemon back one second after a crash, but not after a clean exit. It
leaves every VM running when the daemon stops, so a restart re-adopts them.

The daemon writes to `/var/log/shard/daemon.log`, and `packaging/launchd/shard.newsyslog.conf`
rotates it. At 10 MiB, newsyslog renames the log aside, keeps seven old files, and sends the daemon a
SIGHUP. On that signal the daemon reopens `daemon.log` and keeps running. newsyslog runs once an
hour, so the daemon also caps the log itself between two runs. Past 64 MiB it moves the log to
`daemon.log.overflow`, replacing the previous one, then reopens `daemon.log` and writes a line that
says so. `__USER__` in both files stands for the account that owns the root, and `sed` puts yours in:

```
curl -fsSLO https://raw.githubusercontent.com/presmihaylov/shard/main/packaging/launchd/shard.daemon.plist
curl -fsSLO https://raw.githubusercontent.com/presmihaylov/shard/main/packaging/launchd/shard.newsyslog.conf
sudo install -d -m0755 -o "$USER" /var/log/shard
sed "s/__USER__/$USER/" shard.newsyslog.conf | sudo tee /etc/newsyslog.d/shard.conf >/dev/null
sed "s/__USER__/$USER/" shard.daemon.plist | sudo tee /Library/LaunchDaemons/shard.daemon.plist >/dev/null
sudo launchctl bootstrap system /Library/LaunchDaemons/shard.daemon.plist
```

`launchctl print system/shard.daemon` shows it running, and `shard ls` answers in the same terminal.
Stop any daemon that runs in a terminal first, because two daemons on one root refuse each other over
`daemon.lock`. To remove it:

```
sudo launchctl bootout system/shard.daemon
sudo rm /Library/LaunchDaemons/shard.daemon.plist /etc/newsyslog.d/shard.conf
```

The bootout ends the daemon and nothing else. A running sandbox stays up until a daemon adopts it
again, so to leave the Mac clean, run `shard stop` on each sandbox first.

## What is different from Linux

| | `vz` on a Mac | gVisor on Linux |
|---|---|---|
| Isolation | a micro VM per sandbox, with a Linux kernel of its own | a user-space kernel, `runsc` |
| Syscall cost | native, inside the VM | high on file-heavy work |
| `pause`, `resume`, `fork` | Apple silicon on macOS 14 or later | yes |
| Memory | `--memory` is the VM's memory and is required, and `0` is refused by name. Past it the whole sandbox dies, and restarts on `restart_on_oom` | a cgroup limit. Past it the whole sandbox dies, and restarts on `restart_on_oom` |
| CPUs | `--cpus 0` is one virtual CPU per host CPU, up to the framework's ceiling, and `N` is `N` of them | `--cpus 0` is every host CPU, and `N` is a quota |
| Processes | no bound. A fork bomb stays inside the VM and runs into its memory | `4096` per sandbox |
| Host access | none: no shared folders, no LAN, no host mounts | none |
| Docker inside | yes: `dockerd` as the entrypoint of a `docker:dind` sandbox, IPv4 only | no, use Sysbox on Linux |

`docs/provider.md` has the full matrix across every substrate.

## If it does not start

- `provider vz does not support pause on this host`: the Mac runs macOS 13, or it is an Intel Mac.
  The verb needs Apple silicon on 14.
- `kernel checksum mismatch`: the release no longer serves the bytes this build expects, or the
  `SHARD_KERNEL` file changed. A file under `<root>/kernel/` that was cut short is fetched again on
  its own.
- A VM that never boots on a managed laptop: an MDM profile can block the framework outright. The
  error names `Virtualization.framework`, and nothing works around it except a change to the profile.
- `codesign: command not found`: shard signs the shim on first use with the Command Line Tools.
  `xcode-select --install` installs them. Xcode itself is not needed.
- A daemon restart keeps every sandbox, because the daemon re-adopts each running VM by its shim.
  Sandboxes also survive when the Mac sleeps.

## Appendix: the workaround for a Mac that shard does not support

An Intel Mac, or a Mac on macOS 13, has one way to run shard. Run any Linux VM on the Mac
(UTM, Lima, Parallels, VMware), install `shard` inside it as on any Linux host, and use it from a
shell in the VM. The Linux substrates are then the ones available: gVisor runs every verb, and runc
and Sysbox refuse `pause`, `resume` and `fork` by name (`docs/provider.md`). To drive shard from the
Mac's own terminal instead, expose `shard serve` from the VM and point the native CLI at it. This is
a workaround and not a supported mode.

That one VM is the boundary. Every sandbox shares its kernel, its memory and its disk. The provider
inside still isolates the sandboxes from each other the way it does on any Linux host. But a sandbox
that fills the VM's memory or disk starves the rest, and there is no per-sandbox VM.

The steps below use Lima, because it is a shell script and takes a file.

### The VM

One Lima file covers arm64 on Apple silicon and amd64 on Intel. It has no host mounts, and it
forwards port 2376:

```yaml
# shard.yaml
images:
  - location: "https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-arm64.qcow2"
    arch: "aarch64"
  - location: "https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-amd64.qcow2"
    arch: "x86_64"
cpus: 2
memory: "2GiB"
mounts: []
portForwards:
  - guestPort: 2376
    hostPort: 2376
```

`mounts: []` matters, because Lima mounts `~` into the VM by default, and a VM with the Mac's home
directory inside it cannot serve as a sandbox host. The daemon pulls images over the VM's own
network, so it needs nothing from the Mac's disk.

```
brew install lima
limactl create --name shard shard.yaml
limactl start shard
```

### The binaries

There are three builds. `shard` and `shard-init` for the VM are Linux binaries of the VM's arch. The
`shard` for the Mac is the client alone, so it needs neither cgo nor a shim:

```
GOOS=linux GOARCH=arm64 go build -o bin/shard-linux-arm64 ./cmd/shard
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/shard-init-linux-arm64 ./cmd/shard-init
CGO_ENABLED=0 go build -o bin/shard ./cmd/shard
limactl copy bin/shard-linux-arm64 shard:/tmp/shard
limactl copy bin/shard-init-linux-arm64 shard:/tmp/shard-init
limactl shell shard sudo install -m0755 /tmp/shard /tmp/shard-init /usr/local/bin/
sudo install -m0755 bin/shard /usr/local/bin/shard
```

On an Intel Mac, use `GOARCH=amd64`. A release carries `shard-linux-amd64`, `shard-init-linux-amd64`
and `shard-darwin-<arch>` (`docs/release.md`), so an Intel Mac can skip the Linux builds and an Apple
silicon Mac cannot. The darwin binary is the full daemon, and it serves as the client just the same.
Install the runtime that the provider drives inside the VM. `runc` is `apt-get install runc`. `runsc`
comes from gVisor's own apt repository, and Sysbox from its release package. `docs/provider.md` says
what each one needs from the kernel.

### The daemon, and the front for the Mac's CLI

Inside the VM, the daemon runs as root. The front is only for driving it from the Mac, and a shell in
the VM needs only the daemon. The front runs beside the daemon with a self-signed certificate for
`localhost`, because that is where the Mac reaches the forwarded port:

```
limactl shell shard sudo -i
install -d -m0750 /etc/shard && cd /etc/shard
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 365 \
  -subj /CN=shard -addext subjectAltName=DNS:localhost -keyout serve.key -out serve.crt
umask 077
openssl rand -hex 32 > serve.secret
shard tokens mint --name mac --secret-file serve.secret > mac.token
shard daemon --provider gvisor
```

The front refuses a secret file that everyone can read. A token is a secret too, so the `umask` comes
before both. `--provider runc` or `--provider sysbox` picks one of the other two. The daemon stays in
the foreground, so the front needs a second shell:

```
limactl shell shard sudo shard serve --listen :2376 \
  --cert /etc/shard/serve.crt --key /etc/shard/serve.key --secret-file /etc/shard/serve.secret
```

On a Linux host the two processes run from the systemd units in `packaging/systemd`, and the front
runs unprivileged. Copy that setup for anything that stays up (`docs/daemon.md`). The native daemon
has its own setup, the LaunchDaemon under "Keep it up" above.

### The CLI on the Mac

The native `shard` binary reaches the front with three flags, or with the environment variables
behind them:

```
install -d -m0700 ~/.shard
(umask 077 && limactl shell shard sudo cat /etc/shard/mac.token > ~/.shard/token)
limactl shell shard sudo cat /etc/shard/serve.crt > ~/.shard/ca.pem
export SHARD_REMOTE=https://localhost:2376
export SHARD_TOKEN_FILE=$HOME/.shard/token SHARD_CA_FILE=$HOME/.shard/ca.pem
shard create alpine:3.20 -- sh -c 'echo hello from the VM'
shard logs <id>
shard ls
```

The client refuses a token file that everyone can read, which is why the `umask` is there. Every
verb works this way, exec and `logs -f` included, because the front splices the bytes and the daemon
sees the same requests as it does from the socket. `docs/daemon.md` has the flags, the scopes a token
carries, and how to revoke a token.

### Tearing it down

```
limactl delete -f shard
rm ~/.shard/token ~/.shard/ca.pem
```

Nothing else is left on the Mac, because the images, the sandboxes and the state all lived inside the
VM. Lima itself, and any other VM it runs, stay as they were.
