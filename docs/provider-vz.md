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
cross-compile from Linux. `make build-darwin` runs on a Mac with the Command Line Tools, and
embeds the signed shim and a static linux `shard-init` for the Mac's arch in the daemon, which
installs both under `<root>/vz` on first use; `make check` on Linux compiles the stub and never the
binding.

### One signed shim per sandbox, and the daemon never touches the framework

The daemon starts one `shard-vz-shim` process per sandbox, detached, and speaks to it over a unix
socket in the sandbox's state directory. The shim holds the VM; the daemon holds the record. A daemon
restart re-adopts every running sandbox by that socket (SHARD-235), and only the shim carries the
`com.apple.security.virtualization` entitlement (SHARD-214, `docs/macos-signing.md`). A sandbox whose
shim is gone at that restart is `stopped` with the reason every provider uses, `daemon restarted and
found no process`. A sleep of the host keeps the VM and the shim; if it resets the vsock streams, the
daemon dials the control and the logs streams again while the shim says the VM runs, so `logs -f`
and the events resume where they stopped. The new control connection opens with the guest's state,
and an exit or a restart that landed while no stream was open is recorded from that replay, so
`wait` does not wait for an event that is gone.

This is not only the re-adopt story. **The framework runs at most two VMs in one process.** The
third `start` in a process fails with `VZErrorDomain Code=1, the virtual machine failed to start`,
whatever the machine identifier and however small the VMs. Four processes ran eight VMs at once on
the same Mac without a complaint. A daemon that held its VMs would cap a host at two sandboxes.

Rejected: VMs in the daemon process. Two sandboxes per host, and every sandbox dies with the daemon.

### The kernel is ours, and it is a raw arm64 Image

shard ships one Linux kernel per architecture (SHARD-232, `docs/kernel.md`): virtio-blk, virtio-net,
virtio-vsock, virtio-console, ext4 and overlay built in, no modules, no initrd, versioned and
checksummed with the release, downloaded into the shard root on first use. The Firecracker provider
boots the same amd64 kernel; the arm64 one is this substrate's.

The framework's Linux boot loader takes a raw arm64 `Image` and nothing else. A distribution kernel
does not fit: Alpine's `vmlinuz-virt` is an EFI zboot wrapper (a PE file, `zimg` magic at offset 4,
a gzip payload inside), and the framework refuses it with the same `Code=1` as a hard failure. The
spike unwrapped it by hand; the release kernel is built raw.

Rejected: a distribution kernel with an initrd that loads modules. The wrapper, the module set and
the version drift each become a support case, and the Firecracker kernel is built anyway.

### The root disk is a pure-Go ext4 image, cloned per sandbox

`services/image` unpacks the OCI layers into one ext4 image per image digest, `disks/<digest>.ext4`
beside the rootfs tree, written by `pkg/ext4`, because a Mac has no `mkfs.ext4` and no loop mount.
The image is built from the layer tars, not from the unpacked tree, so it keeps what an unpack on a
Mac loses: the uid and gid, the device nodes and the capability xattrs. `services/image` merges the
layers into one tar stream (whiteouts applied, a hard link kept to the version it took) and
hcsshim's `tar2ext4` lays it down; `ext4.Write` then clears the read-only flag it sets and pads the
inode bitmaps, and `ext4.Grow` can add block groups to a copy offline, up to `ext4.MaxDiskSize`,
128 MiB short of 16 TiB, where its 32-bit block count ends; `sandbox.MaxDiskMiB` is derived from it,
so a `--disk` the daemon accepts is one the writer can grow to. Every sandbox gets an APFS clone of the base
(`clonefile(2)`: instant, and the blocks are shared until written), grown to its `--disk` bound,
attached as virtio-blk, and the clone is the writable layer. `bundle.CloneRootDisk` does both and
reports whether the blocks are shared; on a volume that is not APFS it falls back to a copy, and the
provider says so once in the log (SHARD-215, the wiring and the log line in SHARD-218).

Rejected: a virtiofs share of an unpacked directory. It is the simplest to build and it has no
consistent snapshot: a saved VM state and a directory that keeps changing under it cannot be restored
together, so pause and fork would be wrong on it. Also rejected: squashfs plus an overlay, which needs
squashfs tooling on the host and a second writable disk anyway.

### The channel is vsock, one guest port per stream, and the host opens every connection

`shard-init -transport vsock -root /dev/vda` is the initrd `/init` of every VM (SHARD-216). It moves
onto the root disk, mounts `devpts` for a tty exec, and listens on fixed guest ports. The host
connects to each after boot, retrying until the listener is up:

| Port | Stream | Carries |
|---|---|---|
| 5000 | control | JSON lines: `run` (the resolved entrypoint), `signal`, `stop`, `readdress` in, each numbered and answered with `done` or `failure`; `state`, `ready`, `exit`, `restarts` out |
| 5001 | exec | one connection per exec session: an `ExecHeader` line, then the 8-byte frames the API already uses, plus stream 6 `started` and 7 `resize` |
| 5002 | logs | the entrypoint's stdout and stderr, raw; with no host attached the bytes wait |

Every new control connection hears `state` first (ready, the last exit, the count), written on the
supervisor's own goroutine before any event, so a daemon that restarts, or re-attaches after a
restore, loses nothing. A request returns once the guest has done it: `readdress` answers after the
address and the route are set, so a fork is never exposed on its source address in between. A closed
exec connection kills the command, which is how a cancelled `Exec` ends it. The host writes the exit record and the
count into the same files gVisor's pipe fills, so `Wait`, `ExitStatus` and `inspect` are unchanged.
`services/supervisor` holds the wire and the host client, which the Firecracker provider reuses. The
same binary runs the protocol over `-transport unix:<dir>` in the unit tests, on any OS. The host
side of a tty resize, and `pkg/pty` on darwin, land with the provider (SHARD-218).

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
the host end to the daemon on the `network` verb of the shim socket (`SCM_RIGHTS`). The daemon
terminates the frames in `pkg/netstack`, a userspace stack (gVisor's) that answers for exactly two
addresses, the guest's own and the gateway, and drops everything else: forwarding is off, so a frame
for the LAN, the Mac or the internet is dropped where it arrives. One stack serves the daemon; each
VM is one link on it, with the gateway address on its NIC and a `/32` route back to the guest, so a
guest never sees another guest's frames. The proxy (30080, 30443) and the SHARD-169 resolver (53)
listen on the stack by port alone, the way they listen on the bridge gateway on Linux, and every
frame carries the guest address the SHARD-216 readdress gave it, which is how the broker tells one
sandbox from the next. The stack's NAT table redirects a guest's TCP 80 and 443 onto the proxy
wherever the guest dialed them, the same `dnat` the host chains apply on Linux, so the Host header
and the SNI reach the proxy unchanged. Every other frame is judged before the stack sees it: ARP,
the redirected ports and a served port on the gateway pass, and the rest is dropped and written into
the sandbox's egress log with the shape of a host drop, `rule` `local` for the gateway's own ports,
`private` for the private ranges and `stack` for everything else, at the same two a second with a
burst of ten the chains log at. No packet reaches the Mac, the LAN or the internet except through
the proxy, so the proxy is the policy of record on this substrate, and `docs/egress.md` has the
per-substrate row.

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
- `resume` first replaces the live disk with a fresh APFS clone of the snapshot's `disk.img`, by a
  clone to a temporary name and a rename, then starts a new shim that restores the state file over it
  and resumes. The snapshot is not consumed, and every resume from it starts from the same pair: a
  resume whose shim died after the restore has written to the live disk, and the record still paused
  over the same snapshot, gets the pause-time contents again on the next resume. The fork ticket
  (SHARD-215) ships that repeated-resume case, with a write after round one.
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
9. **A restore needs an unlocked login session.** The helper unwraps the saved state with a key from
   the Secure Enclave, and a locked screen withholds it: every restore then fails with `Code=12,
   permission denied` while a save still succeeds (SHARD-213, macOS 14.6). A headless box that
   never locks is fine; the driver test skips when `ioreg` reports the session locked.
10. **On macOS 26 a guest write to `/dev/console` never reaches the host.** The console file the
    shim reads stays empty while the same guest's writes to `/dev/hvc0` arrive; on macOS 14 both
    arrive. Every guest-side probe and `shard-init` write the virtio console by its own node, `hvc0`.

Not proved yet, and owned by the tickets that need it: a restore over a virtio-blk disk (SHARD-215),
the vsock streams end to end (SHARD-216), the netstack (SHARD-217), and any of this on macOS 13.

## What hypeman contributes

`kernel/hypeman` (MIT, copyright 2025 Kernel) runs this substrate in production, as
`lib/hypervisor/vz` and `cmd/vz-shim`. SHARD-231 read every file of both at `df4d1a56` (2026-09-18)
and decided per file what shard takes. The rule: copy with attribution, never import. No shard
package depends on a hypeman package, and `NOTICE` carries their MIT text. Each origin commit below
is the last commit that touched the file at that revision, so a diff against it shows what shard
changed.

### Taken, as a copy adapted to shard

| Piece | File in hypeman | Origin | Lands in |
|---|---|---|---|
| The device assembly: boot loader, console on file handles, entropy, virtio-blk, vsock, `Validate` before `NewVirtualMachine` | `cmd/vz-shim/vm.go` | `8331138c` | `cmd/shard-vz-shim` (SHARD-213) |
| The machine identifier as base64 data, generated once and handed back so the caller persists it | `cmd/vz-shim/vm.go` `configurePlatform` | `8331138c` | `cmd/shard-vz-shim` (SHARD-213) |
| The shim process shape: config as JSON on argv, restore-or-start, a state watcher that ends the process on `Stopped` or `Error`, SIGTERM stops the VM | `cmd/vz-shim/main.go` | `561e34fd` | `cmd/shard-vz-shim` (SHARD-213) |
| The save and restore split: `darwin && arm64` calls the framework, every other build returns the unsupported error | `cmd/vz-shim/save_restore_arm64.go`, `save_restore_unsupported.go` | `561e34fd` | `pkg/vz` (SHARD-213) |
| The host probe: `kern.osproductversion`, major 14 or later means save and restore exist, an unparsable version means no | `lib/hypervisor/vz/save_restore_support.go`, `save_restore_support_darwin.go` | `5d9eff09` | `pkg/vz` capability probe (SHARD-213) |
| The shim as an embedded binary: `go:embed` the built shim and the plist, write both to a file, `codesign --sign - --entitlements` at first use | `lib/hypervisor/vz/starter.go` `extractShim`, `vz_shim_binary.go`, `vz_entitlements.go` | `840d6235`, `1c69f414` | SHARD-214 |
| The shim start: `Setpgid`, `Wait` in a goroutine, poll the socket, and on a timeout read the shim's log and say whether it exited early | `lib/hypervisor/vz/starter.go` `startShim`, `waitForShim` | `840d6235` | `services/provider/vz` (SHARD-218) |
| The vsock proxy over the shim socket: the client writes `CONNECT <port>\n`, the shim calls `SocketDevices()[0].Connect(port)`, answers `OK <port>\n` and copies both ways; the client keeps its `bufio.Reader` on the connection | `cmd/vz-shim/server.go` `handleVsockConnection`, `lib/hypervisor/vz/vsock.go` | `75c32892`, `1c69f414` | shim socket and `pkg/vz` client (SHARD-216) |

The entitlements plist is copied with one key, `com.apple.security.virtualization`. hypeman's also
grants `network.server` and `network.client`; those are App Sandbox keys and a signed command line
tool outside the sandbox does not need them, which the spike confirmed.

### Written by shard, informed by hypeman

| Piece | What hypeman has | Why shard writes its own |
|---|---|---|
| The shim config | `shimconfig/config.go` (`8331138c`): disks, NAT nets, balloon, Rosetta, snapshot manifest | shard's shim has one disk, one file-handle net device and no balloon, and its record, not a manifest, holds the identifier |
| The shim control channel | `cmd/vz-shim/server.go`, `lib/hypervisor/vz/client.go`: an HTTP API in the shape of cloud-hypervisor's | shard needs to pass the network fd over the socket (`SCM_RIGHTS`), which HTTP cannot carry; the verbs are pause, resume, save, stop and vsock connect, a length-prefixed request each |
| The cpu and memory bounds | `cmd/vz-shim/vm.go` (`8331138c`) `computeMemorySize`, `computeCPUCount`: clamp a request into the framework's `MinimumAllowed`/`MaximumAllowed` | shard's `--memory` and `--cpus` are hard bounds the record and `inspect` report, and a clamp would hand the VM more than the record says; zero keeps the default, and an explicit value outside the framework's range is refused with an error that names the range, as gVisor refuses a request below its minimum. The shim ticket (SHARD-213) ships the boundary tests at both ends of the range |
| Fork | `lib/hypervisor/vz/fork.go` (`561e34fd`): rewrite the snapshot manifest's paths for the target | shard clones the snapshot disk and restores from the record; the lesson taken is that the device configuration, the network device included, must not change between save and restore or the restore fails with `Code=12` |
| The unit tests | `save_restore_support_test.go`, `fork_test.go` (`5d9eff09`, `561e34fd`): the host probe matrix and the manifest path rewrites, against their registry | shard writes its own cases for every adapted behavior, next to the code that lands: the host probe fails closed on every row of the matrix (a non-darwin `GOOS`, a non-arm64 arch, macOS 13, an empty version, a malformed version) in SHARD-213, the snapshot disk pairing and the re-address message in SHARD-215, the vsock handshake in SHARD-216. The conformance suite covers the verbs, not these internals, so without the probe cases SHARD-213 could advertise a verb the host lacks with no focused test failing |

### Not taken

- The NAT network device (`vm.go` `createNATNetworkDevice`, `assignMACAddress`): gotcha 5, the
  frames go to a netstack in the daemon instead.
- The memory balloon: shard's `--memory` is a hard cap, and a balloon would make the VM's footprint
  a moving target on a laptop.
- `lib/instances`, the API, the guest agent and the OpenTelemetry client: shard has its own record,
  API and `shard-init`.
- The Rosetta share (`cmd/vz-shim/rosetta_arm64.go`, `shimconfig/rosetta.go`, `8331138c`) is the
  reference for SHARD-233, which is not approved. Nothing is copied until it is.
- The test files verbatim: they test hypeman's registry. The behaviors they prove get shard-owned
  cases, per the table above.

## Order

SHARD-212 (this page) -> 231 -> 232 -> 213 -> 214 -> 215, 216, 217 -> 218 -> 235 -> 234 -> 219.
