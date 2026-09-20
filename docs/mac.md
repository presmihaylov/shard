# Run shard on a Mac

One binary, one VM per sandbox, every verb. `shard` on a Mac drives Virtualization.framework
itself: the daemon boots a Linux micro VM for each sandbox, over shard's own kernel and a disk it
builds from the image, and the CLI is the same one as on Linux. Nothing to install first, no
Docker, no Homebrew, and the daemon runs as your user.

Needs: Apple silicon, macOS 14 or later. That is the one supported Mac. An Intel Mac is not
supported: the binary builds and the framework boots, but nothing is tested there, `pause`, `resume`
and `fork` refuse by name on every macOS, and a bug on Intel gets no fix. macOS 13 is not supported
either: the three snapshot verbs need the macOS 14 save and refuse by name on 13
(`docs/provider-vz.md`). There is no fallback provider; the appendix is the workaround for both.

## Get the binary

Download `shard-darwin-arm64` from the latest release, put it on the path, and give it a root:

```
curl -fsSLo shard https://github.com/presmihaylov/shard/releases/latest/download/shard-darwin-arm64
chmod +x shard && sudo install -d -m0755 /usr/local/bin && sudo install -m0755 shard /usr/local/bin/shard
sudo install -d -o "$USER" /var/lib/shard
```

A Mac without the developer tools has no `/usr/local/bin`, so the first `install -d` makes it. The
root is where the daemon keeps every record, disk and kernel, and it defaults to `/var/lib/shard`;
the second `install -d` hands it to your user so nothing runs as root. `--root <dir>`
on every command picks another one, and needs no `sudo` at all. A binary a browser fetched carries
the quarantine flag and macOS refuses to run it: `xattr -d com.apple.quarantine shard` clears it.
`curl` sets none.

## Run it

```
shard daemon
```

That is the daemon, in a terminal of its own, and it stays there. On a Mac it picks the `vz`
provider by itself. The daemon builds the provider on the first verb that needs it, not at boot:
that verb writes the signed VM shim and the guest supervisor under the root (`docs/macos-signing.md`)
and fetches the release kernel for this Mac into the root, checked against its hash
(`docs/kernel.md`). A daemon of the same build finds all three in place; a new build replaces the
shim and the supervisor, and the kernel is fetched again only when its tag moves.

In a second terminal:

```
shard create python:3.12 -- python -c 'print(1)'
shard logs <id>
shard exec <id> -- uname -a
shard pause <id>
shard resume <id>
shard stop <id>
```

The image is pulled from the registry and becomes an ext4 disk once; every sandbox over it boots an
APFS clone of that disk, so the second `create` of an image is a boot and nothing more. A sandbox
gets 512 MB by default, and `--memory` is a hard cap, because a VM's memory is real memory on a
laptop. Keep the count of running sandboxes to what the Mac holds: eight of the default size is
4 GB.

The VM has no way onto the LAN. Its network is a file handle into the daemon, where shard's own
netstack terminates it, and the only things it can reach are the egress proxy and the resolver
(`docs/egress.md`). So a `--policy` holds on a Mac the same as on Linux, and a sandbox cannot see
your printer.

## What is different from Linux

| | `vz` on a Mac | gVisor on Linux |
|---|---|---|
| Isolation | a micro VM per sandbox, a Linux kernel of its own | a user-space kernel, `runsc` |
| Syscall cost | native, inside the VM | high on file-heavy work |
| `pause`, `resume`, `fork` | Apple silicon on macOS 14 or later | yes |
| Memory | `--memory` is the VM's memory, 512 MB default; past it the whole sandbox dies, and restarts on `restart_on_oom` | a cgroup limit; past it the whole sandbox dies, and restarts on `restart_on_oom` |
| CPUs | `--cpus 0` is one virtual CPU per host CPU, up to the framework's ceiling; `N` is `N` of them | `--cpus 0` is every host CPU; `N` is a quota |
| Processes | no bound; a fork bomb stays inside the VM and hits its memory | `4096` per sandbox |
| Host access | none: no shared folders, no LAN, no host mounts | none |
| Docker inside | yes: `dockerd` as the entrypoint of a `docker:dind` sandbox, IPv4 only | no; Sysbox on Linux |

`docs/provider.md` has the full matrix across every substrate.

## If it does not start

- `provider vz does not support pause on this host`: macOS 13, or an Intel Mac. The verb needs Apple silicon on 14.
- `kernel checksum mismatch`: a file under `<root>/kernel/` changed. Delete that directory and
  `create` again; the daemon fetches a fresh one.
- A VM that never boots on a managed laptop: an MDM profile can block the framework outright. The
  error names `Virtualization.framework`; there is no workaround short of the profile.
- `codesign: command not found`: the shim is signed on first use with the Command Line Tools.
  `xcode-select --install` puts them on. Xcode itself is not needed.
- A daemon restart keeps every sandbox: it re-adopts each running VM by its shim. A sleep of the
  Mac keeps them too.

## Appendix: the workaround for a Mac shard does not support

An Intel Mac, or macOS 13, has one way onto shard: run any Linux VM on the Mac
(UTM, Lima, Parallels, VMware), install `shard` inside it as on any Linux host, and use it from a
shell in the VM. The Linux substrates are then the ones on offer: gVisor runs every verb, runc and
Sysbox refuse `pause`, `resume` and `fork` by name (`docs/provider.md`). To drive it from the Mac's
own terminal instead, expose `shard serve` from the VM and point the native CLI at it. It is a
workaround, not a supported mode.

**The boundary.** Every sandbox shares that one VM: its kernel, its memory and its disk. The
provider inside still isolates them from each other the way it does on any Linux host, but a
sandbox that fills the VM's memory or disk starves the rest, and there is no per-sandbox VM.

The steps below use Lima, because it is a shell script and takes a file.

### The VM

One Lima file, arm64 on Apple silicon and amd64 on Intel, no host mounts, and port 2376 forwarded:

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

`mounts: []` matters: Lima mounts `~` into the VM by default, and a sandbox host with the Mac's home
directory inside it is not a sandbox host. The daemon pulls images over the VM's own network, so it
needs nothing from the Mac's disk.

```
brew install lima
limactl create --name shard shard.yaml
limactl start shard
```

### The binaries

Three builds: `shard` and `shard-init` for the VM, Linux binaries of the VM's arch, and a `shard`
for the Mac, which is the client alone and needs no cgo and no shim:

```
GOOS=linux GOARCH=arm64 go build -o bin/shard-linux-arm64 ./cmd/shard
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/shard-init-linux-arm64 ./cmd/shard-init
CGO_ENABLED=0 go build -o bin/shard ./cmd/shard
limactl copy bin/shard-linux-arm64 shard:/tmp/shard
limactl copy bin/shard-init-linux-arm64 shard:/tmp/shard-init
limactl shell shard sudo install -m0755 /tmp/shard /tmp/shard-init /usr/local/bin/
sudo install -m0755 bin/shard /usr/local/bin/shard
```

`GOARCH=amd64` on an Intel Mac. A release carries `shard-linux-amd64`, `shard-init-linux-amd64`
and `shard-darwin-<arch>` (`docs/release.md`), so an Intel Mac can skip the Linux builds and an
Apple silicon one cannot; the darwin one is the full daemon, which serves as the client just the
same. The runtime the provider drives is installed inside the VM: `runc`
is `apt-get install runc`; `runsc` comes from gVisor's own apt repository; Sysbox from its release
package. `docs/provider.md` says what each one needs from the kernel.

### The daemon, and the front for the Mac's CLI

Inside the VM, the daemon is root. The front is only for driving it from the Mac; a shell in the VM
needs the daemon alone. It runs beside the daemon with a self-signed certificate for `localhost`,
which is where the Mac reaches the forwarded port:

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

The front refuses a secret file that everyone can read, and a token is a secret too, hence the
`umask` before both. `--provider runc` or `--provider sysbox` picks the other two. The daemon
stays in the foreground, so the front takes a second shell:

```
limactl shell shard sudo shard serve --listen :2376 \
  --cert /etc/shard/serve.crt --key /etc/shard/serve.key --secret-file /etc/shard/serve.secret
```

On a Linux host the two processes run from the systemd units in `packaging/systemd`, and the front
runs unprivileged; that is the shape to copy for anything that stays up (`docs/daemon.md`).

### The CLI on the Mac

The native `shard` binary reaches the front with three flags, or the environment behind them:

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

The client refuses a token file that everyone can read, hence the `umask`. Every verb works this
way, exec and `logs -f` included: the front splices the bytes and the daemon
sees the same requests it does from the socket. `docs/daemon.md` has the flags, the scopes a token
carries, and how to revoke one.

### Tearing it down

```
limactl delete -f shard
rm ~/.shard/token ~/.shard/ca.pem
```

Nothing else is left on the Mac: the images, the sandboxes and the state all lived inside the VM.
Lima itself, and any other VM it runs, stay as they were.
