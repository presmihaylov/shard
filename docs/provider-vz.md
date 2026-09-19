# The VZ guest contract

`vz` is the fourth substrate: one micro VM per sandbox on Apple's Virtualization.framework, driven
by shard itself. The daemon, the CLI, the stores, the API, the broker and the proxy run natively on
macOS; only the substrate is new. This page names every decision SHARD-212 made, the alternative it
rejected, and what a run on two Macs proved. `docs/provider.md` is the contract every substrate
shares; this page says what `vz` does with it.

Target: Apple Silicon, macOS 13 or later. Intel Macs build and boot the framework, get no Rosetta and
no attention. Firecracker stays unsupported on a Mac. Nothing here runs on Linux: `pkg/vz` sits
behind `//go:build darwin` and its stub returns the unsupported-platform error.

## The decisions

### The binding is Code-Hex/vz, and it is cgo

`pkg/vz` wraps `github.com/Code-Hex/vz/v3` (v3.7.1). hypeman runs it in production, it covers every
framework call this substrate needs (boot loader, virtio-blk, virtio-net over a file handle, vsock,
console, entropy, pause, resume, save, restore), and the spike below needed nothing it lacks.

Rejected: binding the ObjC framework directly with cgo. It saves one dependency and costs a
hand-written bridge for every device type, which the binding already carries and tests.

The consequence is that the darwin build is a Mac build: cgo over an ObjC framework does not
cross-compile from Linux. `make build-darwin` runs on a Mac with the Command Line Tools;
`make check` on Linux compiles the stub and never the binding.

### One signed shim per sandbox, and the daemon never touches the framework

The daemon starts one `shard-vz-shim` process per sandbox, detached, and speaks to it over a unix
socket in the sandbox's state directory. The shim holds the VM; the daemon holds the record. A daemon
restart re-adopts every running sandbox by that socket (SHARD-235), and only the shim carries the
`com.apple.security.virtualization` entitlement (SHARD-214).

This is not only the re-adopt story. **The framework runs at most two VMs in one process.** The
third `start` in a process fails with `VZErrorDomain Code=1, the virtual machine failed to start`,
whatever the machine identifier and however small the VMs. Four processes ran eight VMs at once on
the same Mac without a complaint. A daemon that held its VMs would cap a host at two sandboxes.

Rejected: VMs in the daemon process. Two sandboxes per host, and every sandbox dies with the daemon.

### The kernel is ours, and it is a raw arm64 Image

shard ships one Linux kernel per architecture (SHARD-232): virtio-blk, virtio-net, virtio-vsock,
virtio-console, ext4 and overlay built in, no modules, no initrd, versioned and checksummed with the
release, downloaded into the shard root on first use. The Firecracker provider boots the same amd64
kernel; the arm64 one is this substrate's.

The framework's Linux boot loader takes a raw arm64 `Image` and nothing else. A distribution kernel
does not fit: Alpine's `vmlinuz-virt` is an EFI zboot wrapper (a PE file, `zimg` magic at offset 4,
a gzip payload inside), and the framework refuses it with the same `Code=1` as a hard failure. The
spike unwrapped it by hand; the release kernel is built raw.

Rejected: a distribution kernel with an initrd that loads modules. The wrapper, the module set and
the version drift each become a support case, and the Firecracker kernel is built anyway.

### The root disk is a pure-Go ext4 image, cloned per sandbox

`services/image` unpacks the OCI layers into one ext4 image per image digest, written by a Go ext4
writer, because a Mac has no `mkfs.ext4` and no loop mount. Every sandbox gets an APFS clone of that
base disk (`clonefile(2)`: instant, and the blocks are shared until written), attached as virtio-blk,
and the clone is the writable layer. `--disk` is the size of the image. On a volume that is not APFS
the clone falls back to a copy and says so once in the log (SHARD-215).

Rejected: a virtiofs share of an unpacked directory. It is the simplest to build and it has no
consistent snapshot: a saved VM state and a directory that keeps changing under it cannot be restored
together, so pause and fork would be wrong on it. Also rejected: squashfs plus an overlay, which needs
squashfs tooling on the host and a second writable disk anyway.

### The channel is vsock, one guest port per stream, and the host opens every connection

`shard-init` gains a vsock transport next to the pipe one (SHARD-216). It listens on fixed guest
ports and the host connects to each at boot, before the entrypoint starts:

| Port | Stream | Carries |
|---|---|---|
| 5000 | control | ready, the exit record, the restart count, the re-address message of a fork |
| 5001 | exec | one connection per exec session, the 8-byte framing the API already uses |
| 5002 | logs | the entrypoint's stdout and stderr, which the shim writes to the log file |

The host is the only client. The shim never listens on a host port, so a guest process that opens a
vsock connection outward reaches nothing. The exit record travels on the control connection the host
opened at boot, which `shard-init` holds as a file descriptor behind a cleared dumpable flag, the same
way it holds fd 0 on gVisor. A cleared dumpable flag alone is not the boundary: a root process with
`CAP_SYS_PTRACE` in the initial user namespace passes the `/proc` ptrace check anyway, opens
`/proc/1/fd/<n>` and forges the exit record, which is the forge shard-tester proved on Sysbox. gVisor
never grants the guest that capability; the VM would, so `shard-init` drops `CAP_SYS_PTRACE` from
the bounding set of every process it starts, the entrypoint and each exec session, before `exec`. A
bounding set only shrinks, a child user namespace holds no capability over the initial one, and file
capabilities are masked by it, so nothing in the guest regains it. The transport ticket (SHARD-216)
ships the test: guest root opening `/proc/1/fd/<control fd>` gets `EACCES`, on nairiclaw. With that
boundary guest root cannot reach the descriptor and cannot open a second connection the host would
accept, so the exit code stays host-verified, behind the VM boundary, as `docs/provider.md` promises
for a microVM.

Rejected: multiplexing every stream over the virtio console. One serial line, one framing layer to
write and to debug, and a log line and an exec byte fight for it.

### The network is a file-handle device into a netstack in the daemon

The VM's one network device is a `VZFileHandleNetworkDeviceAttachment` over a unix datagram
socketpair (SHARD-217). The shim creates the pair, gives the guest end to the framework, and hands
the host end to the daemon over the shim socket (`SCM_RIGHTS`). The daemon terminates the frames in a
userspace netstack (gVisor's) that answers for exactly two addresses, the proxy and the SHARD-169
resolver, and drops everything else. No packet reaches the Mac, the LAN or the internet except
through the proxy, so the proxy is the policy of record on this substrate, and `docs/egress.md`
gains a per-substrate row.

Rejected: the framework's NAT attachment. It gives the guest `bridge100` at `192.168.64.1/24` with a
route to the LAN and the Mac, and the only filter for it is `pf`, which needs root and is host state
shard does not own. hypeman uses it and had a whole-egress outage from it (their issue 358). Also
rejected: the netstack inside the shim. The shim is mechanism, and the frames would need a second hop
to reach the proxy anyway.

### Pause, resume and fork are save and restore, and the state file is reusable

- `pause` pauses the VM, saves its state to `<snapshot dir>/vm.vzvmstate`, stops the VM, and then
  takes an APFS clone of the quiescent disk as `<snapshot dir>/disk.img` beside it; the shim exits.
  The memory is freed, as the verb promises on gVisor; the live disk stays where it is. The two
  files are one snapshot: the memory and the disk of the same instant.
- `resume` starts a new shim that restores the state file over the live disk and resumes. The
  snapshot is not consumed.
- `fork` clones the snapshot's `disk.img`, never the live disk, starts a new shim that restores the
  same state file over that clone, resumes, then sends one re-address message on the control port so
  the guest drops the source's address and takes its own (hypeman's issue 423 is a fork that answers
  on the old IP). A source that resumed and wrote to its disk since the pause forks from the
  pause-time pair, the way gVisor forks the layers `Pause` exported; the fork ticket (SHARD-215)
  ships that resumed-source regression case.

A restore refuses a VM whose configuration differs from the saved one, and the machine identifier
is part of that configuration. The framework generates a fresh identifier per configuration, so the
provider persists each sandbox's identifier in its state directory and reuses it on every resume and
fork. The spike found this the hard way: `Code=12, invalid argument`.

Rejected: an in-memory pause (the framework's `pause` alone). shard deleted the in-memory pause so
the verb means one thing on every substrate: a snapshot on disk and the memory given back.

### The capability list

| Verb | macOS 14+ | macOS 13 |
|---|---|---|
| `create`, `start`, `stop`, `rm`, `clone`, `exec`, `logs`, `inspect` | yes | yes |
| `pause` | yes | **no** |
| `resume` | yes | **no** |
| `fork` | yes | **no** |

Save and restore are macOS 14 APIs. A pause that frees memory needs the save, so on macOS 13 all
three optional verbs are `false` and refuse by name, `provider vz does not support pause on this
host`, the same as Sysbox. `--memory` defaults to 512 MB on this substrate and is a hard cap, because
a VM's memory is real memory on a laptop.

## What the spike proved

A throwaway Go program over Code-Hex/vz v3.7.1, ad-hoc signed with the virtualization entitlement,
booting the Alpine 3.22 arm64 `virt` kernel (unwrapped to a raw Image) and initramfs, 1 cpu, 512 MB,
a virtio console on a pipe pair, an entropy device, a vsock device, and on the last runs a
file-handle network device. It ran on 2026-09-19 on a MacBook (M-series, macOS 14.6.1) and on
`nairiclaw` (Mac mini M2, macOS 26.6) with the same results:

1. Boot to the initramfs shell in about 2 s; the guest answers on the console.
2. Pause and save: 24 MB of state for a 512 MB VM, in about 280 ms. The source resumes after the save
   and answers.
3. Restore into a second VM while the source runs: about 200 ms, the copy answers.
4. **One saved state restores any number of times.** Three processes restored the same file at once,
   with the source still running, each copy answered, and a fourth restore after the source stopped
   still worked. The file is not consumed. Fork is therefore save once, restore N, and the
   `Capabilities.Fork` flag is `true` on macOS 14 and later.
5. The two copies diverge: a file written in one is not in the other.
6. A file-handle network device survives save and restore: the restored guest brings `eth0` up and
   its frames arrive on the host end of the socketpair. Restore with the device attached takes about
   1.2 s instead of 200 ms.
7. **Two VMs per process.** The third VM in one process fails to start, with the same identifier or
   distinct ones, restored or cold-booted. Eight VMs over four processes run together.
8. A restore with a fresh machine identifier is refused with `Code=12, invalid argument`.

Not proved yet, and owned by the tickets that need it: a restore over a virtio-blk disk (SHARD-215),
the vsock streams end to end (SHARD-216), the netstack (SHARD-217), and any of this on macOS 13.

## What hypeman contributes

SHARD-231 decides per file what shard copies from `kernel/hypeman` (`lib/hypervisor/vz`, `cmd/vz-shim`,
MIT) and lists each piece here with its origin commit. The candidates: the shim with a runtime
`codesign --sign -`, the per-VM shim socket and the reconnect, the vsock handshake, the arm64-only
save and restore split, the entitlements plist. Their NAT networking, their instance store, their API
and their guest agent are not taken. Copy with attribution, never import.

## Order

SHARD-212 (this page) -> 231 -> 232 -> 213 -> 214 -> 215, 216, 217 -> 218 -> 235 -> 234 -> 219.
