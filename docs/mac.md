# Macs shard does not support, and the workaround

`shard` supports one Mac: Apple silicon on macOS 14 or later, over the native `vz` provider
(`docs/provider-vz.md`). An Intel Mac is not supported: the binary builds and the framework boots,
but nothing is tested there, the snapshot verbs refuse by name on every macOS, and a bug on Intel
gets no fix. macOS 13 is not supported either: `pause`, `resume` and `fork` need the macOS 14 save,
so on 13 they refuse by name.

There is no fallback provider. The one way to run shard on such a Mac is the workaround below: a
Linux VM with shard inside it. It is a workaround, not a supported mode.

## Workaround: shard inside one Linux VM

Run any Linux VM on the Mac (UTM, Lima, Parallels, VMware), install `shard` inside it as on any
Linux host, and use it from a shell in the VM. The Linux substrates are then the ones on offer:
gVisor runs every verb, runc and Sysbox refuse `pause`, `resume` and `fork` by name
(`docs/provider.md`). To drive it from the Mac's own terminal instead, expose
`shard serve` from the VM and point the native CLI at it.

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
same. The runtime the provider drives is installed
inside the VM: `runc` is `apt-get install runc`; `runsc` comes from gVisor's own apt repository;
Sysbox from its release package. `docs/provider.md` says what each one needs from the kernel.

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
way, exec and `logs -f` included: the front splices the bytes and the daemon sees the same requests
it does from the socket. `docs/daemon.md` has the flags, the scopes a token carries, and how to
revoke one.

### Tearing it down

```
limactl delete -f shard
rm ~/.shard/token ~/.shard/ca.pem
```

Nothing else is left on the Mac: the images, the sandboxes and the state all lived inside the VM.
Lima itself, and any other VM it runs, stay as they were.
