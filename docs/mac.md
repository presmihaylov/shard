# shard on a Mac: the other providers, in one Linux VM

The native Mac path is the `vz` provider: `make build-darwin` on a Mac, `shard daemon` on the Mac
itself, one Virtualization.framework VM per sandbox (`docs/provider-vz.md`). This page is the other
path: gVisor, Sysbox or runc on a Mac, for a provider `vz` is not, or a macOS the framework binding
does not cover. It runs the whole Linux stack inside one Lima VM, exposes `shard serve` from it, and
drives it with the native CLI on the Mac.

**The boundary.** Every sandbox shares that one VM: its kernel, its memory and its disk. The
provider inside still isolates them from each other the way it does on any Linux host, but a
sandbox that fills the VM's memory or disk starves the rest, and there is no per-sandbox VM. The
`vz` provider is the per-sandbox shape.

## The VM

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

## The binaries

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

`GOARCH=amd64` on an Intel Mac. A release carries the same three, `shard-linux-<arch>`,
`shard-init-linux-<arch>` and `shard-darwin-<arch>` (`docs/release.md`); the darwin one is the full
daemon, which serves as the client just the same. The runtime the provider drives is installed inside the VM: `runc`
is `apt-get install runc`; `runsc` comes from gVisor's own apt repository; Sysbox from its release
package. `docs/provider.md` says what each one needs from the kernel.

## The daemon and the front

Inside the VM, the daemon is root, and the front runs beside it with a self-signed certificate for
`localhost`, which is where the Mac reaches the forwarded port:

```
limactl shell shard sudo -i
install -d -m0750 /etc/shard && cd /etc/shard
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 365 \
  -subj /CN=shard -addext subjectAltName=DNS:localhost -keyout serve.key -out serve.crt
(umask 077 && openssl rand -hex 32 > serve.secret)
shard tokens mint --name mac --secret-file serve.secret > /tmp/token
shard daemon --provider runc
```

The front refuses a secret file that everyone can read, hence the `umask`. `--provider gvisor` or
`--provider sysbox` picks the other two. The daemon stays in the foreground, so the front takes a
second shell:

```
limactl shell shard sudo shard serve --listen :2376 \
  --cert /etc/shard/serve.crt --key /etc/shard/serve.key --secret-file /etc/shard/serve.secret
```

On a Linux host the two processes run from the systemd units in `packaging/systemd`, and the front
runs unprivileged; that is the shape to copy for anything that stays up (`docs/daemon.md`).

## The CLI on the Mac

The native `shard` binary reaches the front with three flags, or the environment behind them:

```
install -d -m0700 ~/.shard
(umask 077 && limactl shell shard sudo cat /tmp/token > ~/.shard/token)
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

## Tearing it down

```
limactl delete -f shard
rm ~/.shard/token ~/.shard/ca.pem
```

Nothing else is left on the Mac: the images, the sandboxes and the state all lived inside the VM.
Lima itself, and any other VM it runs, stay as they were.
