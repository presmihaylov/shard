# The VZ guest contract

`vz` is the fourth substrate. It runs one micro VM per sandbox on Apple's Virtualization.framework,
and shard drives the framework itself. The daemon, the CLI, the stores, the API, the broker and the
proxy run natively on macOS, so only the substrate is new. This page names every decision SHARD-212
made and the alternative each one rejected, and it records what a run on two Macs proved.
`docs/provider.md` is the contract that every substrate shares, and this page says what `vz` does
with it.

The target is Apple Silicon on macOS 14 or later. Intel Macs can build and boot the framework, but
shard does not support them: they get no Rosetta, no `pause`, `resume` or `fork`, and no development
attention. macOS 13 runs every verb except `pause`, `resume` and `fork`, and it is not supported
either. [A Mac that cannot host sandboxes](https://useshards.com/docs/guides/mac/#a-mac-that-cannot-host-sandboxes) has the workaround for both. Firecracker stays unsupported on a Mac. Nothing
on this page runs on Linux, because `pkg/vz` sits behind `//go:build darwin` and its stub returns
the unsupported-platform error.

## The decisions

### The binding is Code-Hex/vz, and it is cgo

`pkg/vz` wraps `github.com/Code-Hex/vz/v3` (v3.7.1). hypeman runs this binding in production. It
covers every framework call this substrate needs (boot loader, virtio-blk, virtio-net over a file
handle, vsock, console, entropy, pause, resume, save, restore), and the spike below needed nothing
that it lacks.

The build uses a copy at `third_party/vz`, which `go.mod` names in a `replace` line. Upstream wraps
the fd of each vsock connection in a file and closes it, but the connection object owns that fd and
closes it again when it is destroyed. Under a burst of connects the second close hit the shim's own
socket, and the shim stopped answering. The copy dups the fd and closes only the dup; its `UPSTREAM`
file names the version and the patch, and `diff -r` against the module cache shows nothing else.

Rejected: binding the ObjC framework directly with cgo. That saves one dependency, but it costs a
hand-written bridge for every device type, and the binding already carries and tests those bridges.

As a result, the darwin build is a Mac build, because cgo over an ObjC framework does not
cross-compile from Linux. `make build-darwin` runs on a Mac with the Command Line Tools. It embeds
the signed shim and a static linux `shard-init` for the Mac's arch in the daemon, and the daemon
installs both under `<root>/vz` on first use. `make check` on Linux compiles the stub and never the
binding. CI builds and unit-tests the darwin binaries on a macOS runner. The tests that boot a VM
run by hand before a release (`docs/release.md`).

### One signed shim per sandbox, and the daemon never touches the framework

The daemon starts one detached `shard-vz-shim` process per sandbox and speaks to it over a unix
socket in the sandbox's state directory. The shim holds the VM, and the daemon holds the record. A
daemon restart re-adopts every running sandbox by that socket (SHARD-235). Only the shim carries the
`com.apple.security.virtualization` entitlement (SHARD-214, `docs/macos-signing.md`). A sandbox whose
shim is gone at that restart is `stopped`, with the reason every provider uses: `daemon restarted and
found no process`. A shim that goes silent for 5 s is never killed for that silence, whether the
daemon holds it or a restart meets it only by its socket. Its sandbox reads `unresponsive` with the
shim's pid until a probe answers, and `stop` kills it with no grace (SHARD-421, SHARD-422). A shim
that a restart found silent gets one request that every later probe shares, so its socket queue
never fills. A queue that fills anyway refuses every dial, as the socket of a dead shim does. So the
daemon reads a refused shim as gone only once the pid and the start time that the last attach wrote
to `shim.json` are gone (SHARD-423). `stop` kills the shim alone, through a pin that the kernel
holds on the process. The daemon pins the pid and then checks the start time, or it pins the pid
behind the socket and then proves it with a second dial, so a pid that the kernel gave to another
process is never signalled. A shim that an older daemon booted has no `shim.json`. The daemon finds
such a shim as a process of its own user that the kernel says runs from the installed shim's path,
with a `-config` that names the socket. An argv alone is never enough. When the host sleeps, the VM
and the shim stay. If the sleep resets the vsock streams, the daemon dials the control and the logs
streams again while the shim reports that the VM runs, so `logs -f` and the events resume where they
stopped. The new control connection opens with the guest's state. An exit or a restart that happened
while no stream was open is recorded from that replay, so `wait` does not wait for an event that is
gone.

The shim lives exactly as long as its VM. A vsock connect to a port the guest does not serve never
calls back (the framework "does nothing" for it), so the shim bounds every connect at 5 seconds and
then answers that nothing listens. A `create` whose guest never reaches the supervisor fails within
its 30 second grace. It stops the VM, waits for the shim to go, and kills the shim by pid if it is
still there 10 seconds later. That cleanup runs on its own clock, so it carries on past a canceled
`create` and past a failed stop, and it reports a shim that outlives the kill (SHARD-255). SIGTERM
to a shim force-stops its VM. If the framework never reports the stop, the shim ends after 5
seconds anyway.

The re-adopt is not the only reason for a process per sandbox. **The framework runs at most two VMs
in one process.** The third `start` in a process fails with
`VZErrorDomain Code=1, the virtual machine failed to start`, whatever the machine identifier and
however small the VMs are. Four processes ran eight VMs at once on the same Mac with no complaint.
A daemon that held its own VMs would cap a host at two sandboxes.

Rejected: VMs in the daemon process. That allows two sandboxes per host, and every sandbox dies with
the daemon.

### The kernel is ours, and it is a raw arm64 Image

shard ships one Linux kernel per architecture (SHARD-232, `docs/kernel.md`). It has virtio-blk,
virtio-net, virtio-vsock, virtio-console, ext4 and overlay built in, with no modules. An initrd
carries `shard-init`. The kernel is versioned and checksummed with the release, and shard downloads
it into the shard root on first use. The Firecracker provider boots the build for its host's
architecture, arm64 or amd64, and this substrate boots the arm64 build.

Docker runs inside a VM (SHARD-247). The kernel carries everything that `dockerd` and `runc` assert
at start: netfilter with conntrack and NAT, nf_tables and the xtables compat layer (so either
`iptables` flavour works), the bridge with its netfilter hook, veth, overlay, cgroup v2 with the
cpu, memory, pids and device controllers, user namespaces and seccomp. IPv6 is off in this kernel,
so `dockerd` runs IPv4 only. Before the entrypoint starts, `shard-init` mounts `cgroup2` on
`/sys/fs/cgroup`, a `tmpfs` on `/dev/shm` and `mqueue` on `/dev/mqueue`. An image that ships
`dockerd` can therefore start it as the entrypoint or under one. A container's packets leave over
the bridge, masqueraded to the guest's address, and cross the stack as the guest's own. Traffic to
80 and 443 goes to the proxy. The rest is judged and dropped like any other frame, and written to
the egress log under the sandbox.

The framework's Linux boot loader takes only a raw arm64 `Image`. A distribution kernel does not
fit. Alpine's `vmlinuz-virt` is an EFI zboot wrapper (a PE file, `zimg` magic at offset 4, a gzip
payload inside), and the framework refuses it with the same `Code=1` as a hard failure. The spike
unwrapped it by hand, and the release kernel is built raw.

Rejected: a distribution kernel with an initrd that loads modules. The wrapper, the module set and
the version drift would each become a support case, and shard builds the Firecracker kernel anyway.

### The root disk is a pure-Go ext4 image, cloned per sandbox

`services/image` unpacks the OCI layers into one ext4 image per image digest, `disks/<digest>.ext4`,
beside the rootfs tree. `pkg/ext4` writes it, because a Mac has no `mkfs.ext4` and no loop mount.
The image is built from the layer tars and not from the unpacked tree, so it keeps what an unpack on
a Mac loses: the uid and gid, the device nodes and the capability xattrs. `services/image` merges the
layers into one tar stream (whiteouts applied, a hard link kept to the version it took), and
hcsshim's `tar2ext4` lays it down. `ext4.Write` then clears the read-only flag that hcsshim sets and
pads the inode bitmaps. It also widens every block group to 8192 inodes, which is mke2fs's one
per 16 KiB. `tar2ext4` sizes the table to the tar, so on its own it leaves a small `--disk` with
fewer than 16 spare inodes (SHARD-254).

`ext4.Grow` can add block groups to a copy offline, up to `ext4.MaxDiskSize`. That limit is
128 MiB short of 16 TiB, where the 32-bit block count ends, and `sandbox.MaxDiskMiB` is derived from
it. The provider refuses a `--disk` whose last block group cannot hold its own metadata, so the
writer can grow to any bound the daemon accepts. The exception is a bound under the image's own
disk, which only the clone finds (SHARD-280).

Every sandbox gets an APFS clone of the base (`clonefile(2)`: instant, and the blocks are shared
until written). The clone is grown to the sandbox's `--disk` bound and attached as virtio-blk, and
it is the writable layer. `bundle.CloneRootDisk` does both the clone and the grow, and it reports
whether the blocks are shared. On a volume that is not APFS it falls back to a full copy. A create
from a snapshot clones the snapshot's disk the same way and grows it to a larger `--disk`, by the
rules in `docs/provider.md` (SHARD-476).

Rejected: a virtiofs share of an unpacked directory. It is the simplest to build, but it has no
consistent point-in-time copy. A saved VM state and a directory that keeps changing under it cannot
be restored together, so pause and fork would be wrong on it. Also rejected: squashfs plus an
overlay, which needs squashfs tooling on the host and a second writable disk anyway.

### The channel is vsock, one guest port per stream, and the host opens every connection

`shard-init -transport vsock -root /dev/vda` is the initrd `/init` of every VM (SHARD-216). It moves
onto the root disk and mounts `devpts` for a tty exec. It also mounts `cgroup2`, `/dev/shm` and
`mqueue` for a container runtime. Then it listens on fixed guest ports. The host connects to each
port after boot and retries until the listener is up:

| Port | Stream | Carries |
|---|---|---|
| 5000 | control | JSON lines. In: `run` (the resolved entrypoint), `signal`, `stop`, `stop-app`, `readdress`, `reseed`, `freeze`, `thaw` and `kill`, each numbered and answered with `done` or `failure`. Out: `state`, `ready`, `exit`, `restarts`, `oom` and `supervisor-failed`. The host refuses a line past 1 MiB. It redials after 100 ms, waits twice as long after each refusal up to 2 s, and starts from 100 ms again after a quiet minute (SHARD-408) |
| 5001 | exec | one connection per exec session. It carries an `ExecHeader` line, then the 8-byte frames the API already uses, plus stream 6 `started`, 7 `resize` and 8 `cancel` |
| 5002 | logs | the entrypoint's stdout and stderr, in the protocol that the guest's `state` names as `logs`. At version 1 the guest opens with two big-endian uint64s: the offset of the oldest output byte it holds and the offset of the next one. The host answers with one uint64, the byte to resume from. Then it reads raw bytes and acks each write to `output.log` with the offset after that write. The guest holds up to 1 MiB that no host has acked, so a daemon restart loses nothing and repeats nothing. `output.cursor` maps the file to the offsets, and a fresh boot drops it. A `state` with no `logs` comes from a guest older than the protocol, so the host lands every byte raw and sends nothing back. An unknown version marks the sandbox lost (SHARD-243) |

A files operation has no port of its own. It is an exec of `/.shard/init files` on 5001, as on
every provider. A put lands under a temp name beside the target. Once the bytes, the mode and the
sync are in, it renames the file into place and then syncs the directory. A copy that dies midway
therefore leaves the old file whole and leaves no partial file. A get streams to the end of the
file, whatever size its stat reports. Neither operation takes a directory.

Every new control connection hears `state` first (ready, the last exit, the count). The supervisor
writes it on its own goroutine before any event, so a daemon that restarts, or that re-attaches
after a restore, loses nothing. A request returns once the guest has done it. `readdress` answers
after the address and the route are set, so a fork is never exposed on its source address in
between. A cancelled `Exec` sends a `cancel` frame, which kills the command. When an exec
connection only closes, the daemon went away, so the command runs on and its output drains, as on
gVisor (SHARD-270). The host writes the exit record and the count into the same files that gVisor's
pipe fills, so `Wait`, `ExitStatus` and `inspect` work as they do on gVisor. `services/supervisor`
holds the wire and the host client, and the Firecracker provider reuses both. In the unit tests the
same binary runs the protocol over `-transport unix:<dir>`, on any OS. The host side of a tty
resize, and `pkg/pty` on darwin, ship with the provider (SHARD-218).

On gVisor, a supervisor that fails its own bookkeeping exits 125, and the host reads that back with
`runsc wait`. A VM halts when PID 1 exits, and the code is lost with it. So `shard-init` first sends
`supervisor-failed` on the control connection, with the reason and the exit code 125. If no host is
attached, it waits up to ten seconds for one to attach, then exits (SHARD-42). The Firecracker
provider records the 125 as the sandbox exit and the reason as its stopped reason. `inspect` and
`list` show that as `shard-init failed: <reason>` until the next start (SHARD-290). A failure at boot,
before any listener exists, opens the control connection with the same message. A Firecracker start
then answers with the reason and the 125 at once, instead of after the 30 second grace (SHARD-416).
The vz provider records a failure at boot the same way (SHARD-418). It records a death after boot as
Firecracker does, and a stop returns only once that report has landed (SHARD-476).

The host is the only client. The shim never listens on a host port, so a guest process that opens a
vsock connection outward reaches nothing. The exit record travels on the control connection that
the host opened at boot. `shard-init` holds that connection as a file descriptor behind a cleared
dumpable flag, the same way it holds fd 0 on gVisor. A cleared dumpable flag alone is not the
boundary. A root process with `CAP_SYS_PTRACE` in the initial user namespace passes the `/proc`
ptrace check anyway, opens `/proc/1/fd/<n>` and forges the exit record. shard-tester proved that
forge on Sysbox. gVisor never grants the guest that capability, but the VM would. So `shard-init`
drops `CAP_SYS_PTRACE` from its own bounding set in PID 1 and re-execs itself. The entrypoint and
every exec session inherit that reduced bounding set. A bounding set only shrinks, a child user
namespace holds no capability over the initial one, and the bounding set masks file capabilities,
so nothing in the guest regains it. The transport ticket (SHARD-216) ships the test: on nairiclaw,
guest root that opens `/proc/1/fd/<control fd>` gets `EACCES`. With that boundary, guest root cannot reach the
descriptor and cannot open a second connection that the host would accept. The exit code therefore
stays host-verified, behind the VM boundary, as `docs/provider.md` promises for a microVM.

Rejected: multiplexing every stream over the virtio console. That leaves one serial line and one
framing layer to write and to debug, and a log line and an exec byte would fight for the line.

### The network is a file-handle device into a netstack in the daemon

The VM's one network device is a `VZFileHandleNetworkDeviceAttachment` over a unix datagram
socketpair (SHARD-217). The shim creates the pair and gives the guest end to the framework. It hands
the host end to the daemon on the `network` verb of the shim socket (`SCM_RIGHTS`). The daemon
terminates the frames in `pkg/netstack`, a userspace stack (gVisor's) that answers for the gateway
and forwards nothing by itself. The daemon dials a flow to any other address only when the egress
judge allows it, and otherwise the flow is dropped where it arrives. One stack serves the daemon.
Each VM is one link on it, with the gateway address on its NIC and a `/32` route back to the guest,
so a guest never sees another guest's frames. The proxy (30080, 30443) and the SHARD-169 resolver
(53) listen on the stack by port alone, the way they listen on the bridge gateway on Linux. Every
frame carries the guest address that the SHARD-216 readdress gave it, and that address is how the
broker tells one sandbox from the next.

A fronted guest is one with a policy or a secret. The stack's NAT table redirects a fronted guest's
TCP 80 and 443 onto the proxy, wherever the guest dialed them. This is the same `dnat` that the host
chains apply on Linux, so the Host header and the SNI reach the proxy unchanged. Any other guest's
80 and 443 are judged like every other port (SHARD-294).

Every other frame is judged before the stack sees it. ARP, a fronted guest's redirected ports and a
served port on the gateway pass. A TCP or UDP flow to any other address goes to the stack's
forwarders, where the daemon's judge rules on it with the same compiled chains that the Linux
ruleset is built from (SHARD-246). The judge answers as the `egress` chain does. The private floor
drops first. A sandbox without a policy reaches everything else, and a sandbox with one gets its
rules in order and the default drop after them. The daemon dials an allowed flow and splices it to
the guest. It carries a TCP half-close across and ages out a UDP flow after thirty idle seconds. A
refused flow gets no answer, as a netfilter drop gives none.

Every drop is written into the sandbox's egress log with the shape of a host drop. The `rule` field
names the rule that refused a judged flow, and other drops carry a fixed value in it. `private`
marks the floor. `local` marks the gateway's own ports and any address the Mac owns, which the input
chain refuses on Linux. `unapplied` marks a flow that arrived before the daemon's first apply, since
a VM adopted at startup gets no window. `limit` marks a flow, a proxied connection included, past
the 1024 that a sandbox may hold open or the 4096 that the stack may hold. `redirect` marks a
fronted guest's 80 or 443 that connection tracking kept off the proxy, because it first saw the
flow before the guest was fronted. `stack` marks a frame the forwarders never take: ICMP, a
fragment, or a port the daemon serves reached on an address other than the gateway. These log
lines have the same bound as the chains: two a second, with a burst of ten. Nothing reaches the
Mac, the LAN or the internet except through the proxy or a flow the policy allowed.
[On a Mac](https://useshards.com/docs/security/egress/#on-a-mac) covers vz on the egress page.

Each end of the socketpair has a 1 MiB send buffer and a 4 MiB receive buffer, which is the four to
one ratio Apple asks for. At the macOS default of 4 KiB, a full peer refuses the third 1514 byte
frame with `ENOBUFS`, and a 1 MB download filled it (SHARD-384). When the guest end still has no
room for a frame, the link treats that as back-pressure and not as a fault. It tries the frame five
times over about 1.5 ms, then drops it for TCP to send again. The pump never ends on `ENOBUFS` or
`EAGAIN`. The daemon log counts the dropped frames per sandbox: at the first drop, then at most once
in ten seconds, and when the link closes.

Rejected: the framework's NAT attachment. It gives the guest `bridge100` at `192.168.64.1/24` with a
route to the LAN and the Mac. The only filter for it is `pf`, which needs root and is host state
that shard does not own. hypeman uses the NAT attachment and had a whole-egress outage from it
(their issue 358). Also rejected: the netstack inside the shim. The shim is mechanism only, and the
frames would need a second hop to reach the proxy anyway.

### Pause, resume and fork are save and restore, and the state file is reusable

- `pause` asks `shard-init` to freeze the guest, pauses the VM, saves its state to
  `<checkpoint dir>/vm.vzvmstate`, and takes an APFS clone of the quiescent disk as
  `<checkpoint dir>/disk.img` while the VM stays paused. It installs the checkpoint, then stops
  the VM and ends the shim. The memory is freed, as the verb promises on gVisor, and the live disk
  stays where it is. The two files are one checkpoint: the memory and the disk of the same instant.
  The disk is copied apart from the memory, so the pause freezes the guest first (SHARD-296).
  `shard-init` first freezes the guest's processes, through `cgroup.freeze` on the sandbox cgroup, and then freezes the root. `FIFREEZE`
  flushes the root and holds every write until the thaw, so no write lands between the flush and the
  pause. A sync alone leaves that window open, and with only a sync a writer in a loop tore the copy
  of its file on every try. The cgroup freeze comes first because a writer that a frozen root holds
  would sleep where no cgroup freeze reaches it. The thaw runs in the reverse order.
- Every path that runs a frozen guest again thaws it. If a pause fails at or after the freeze, it
  resumes the VM (when the pause got that far) and then thaws. The state that `shard-init` replays
  on a new control connection says whether the guest is frozen, and the host that reads it thaws
  the guest. This happens after a resume, after a fork (before the re-address), after a daemon died
  between the freeze and the pause, and after a control connection dropped with the freeze's answer.
  A connection dialed again while a pause is still in flight leaves the guest frozen for that pause.
  `shard-init` undoes a freeze whose host was replaced before the answer, since that host's replay
  may predate the freeze. A stop thaws the guest before it signals the entrypoint. A guest whose
  `shard-init` predates the freeze refuses it, and the pause fails with that refusal. A checkpoint
  taken before the freeze existed never says frozen, and it resumes the same way it always did.
- `resume` first replaces the live disk with a fresh APFS clone of the checkpoint's `disk.img`. It
  clones to a temporary name and then renames. Next it starts a new shim, which restores the state
  file over the new disk and resumes. The checkpoint is not consumed, and every resume from it starts
  from the same pair. When a resume's shim dies after the restore has written to the live disk, and
  the record is still paused over the same checkpoint, the next resume gets the pause-time contents
  again. The fork ticket (SHARD-215) ships that repeated-resume case, with a write after round one.
- `fork` takes a running source (SHARD-457) and runs it on (SHARD-463). It asks `shard-init` to
  freeze the guest for a fork, pauses the VM, and writes the same pair a pause writes into
  `<fork dir>/capture`. Then it resumes the source in the same shim and thaws it, whether the
  capture worked or not. The source's record does not change. The fork clones the capture's
  `disk.img` and never the live disk. It starts a new shim that restores the same state file over
  that clone and resumes. Then it sends one re-address message on the control port, so the guest
  drops the source's address and takes its own (hypeman's issue 423 is a fork that answers on the
  old IP). The capture then goes. A fork id that is alive is refused before any freeze.
- A save may reset every vsock stream of the source's guest. If the thaw finds the control stream
  gone, the daemon dials it again and thaws the guest over the new one. If the guest takes no new
  control stream within 30 s, the fork fails with `sandbox <id> stays frozen after the fork`, and
  the follower tries for up to another 30 s. If that also fails, the follower ends and reports the
  source stopped, although the VM can still run with its guest filesystem frozen. Before that
  deadline, `inspect` reads the source running, and an `exec` is refused with `a fork holds the
  sandbox frozen, and nothing starts in it until that ends: run the command again`. An `exec` whose
  stream the save reset fails with an error that names the fork, and its command runs on in the
  sandbox with no reader.
- A daemon killed inside a fork's capture leaves the source's VM paused, or its guest frozen, under
  a record that says running. The next daemon resumes the VM when it adopts the shim, then reseeds
  and thaws the guest, so the fork needs no marker of its own. A completed `pause` has ended its
  shim, so a paused record has no VM to resume. The next daemon's start also tears down the fork it
  dropped, its capture included, and that fork's record says failed until `rm`.

A restore refuses a VM whose configuration differs from the saved one, and the machine identifier
is part of that configuration. The framework generates a fresh identifier per configuration, so the
provider persists each sandbox's identifier in its state directory and reuses it on every resume and
fork. The spike learned this from a failed restore: `Code=12, invalid argument`.

Every restore of one state file also wakes with the same kernel crng key, so a resumed source and
its forks would read the same `/dev/urandom` bytes until the guest's next timed reseed. VZ has no
vmgenid device to tell the guest about the restore. So every `resume` and `fork` sends 32 bytes
from the host's `crypto/rand` on the control port, and `shard-init` writes them into the input pool
and forces a rekey with `RNDRESEEDCRNG` (SHARD-293). The seed goes in while every guest process is
still frozen from the pause, and the thaw comes only after it, so no process in any copy reads a
byte of the saved key (SHARD-310). A save from an older `shard-init` restores unfrozen. It still
gets the seed, but with the 5 to 9 ms window of SHARD-293. `shard-init` itself sits in a sibling
cgroup, `init`, so it can answer while the guest is frozen. Only the kernel's generator is rekeyed.
A process that seeded its own generator before the pause carries that state into every copy.

Rejected: an in-memory pause (the framework's `pause` alone). shard has no in-memory pause, so the
verb means one thing on every substrate: a checkpoint on disk and the memory given back.

### The capability list

| Verb | macOS 14+ | macOS 13 |
|---|---|---|
| `create`, `start`, `stop`, `remove`, `snapshot create`, `exec`, `logs`, `inspect` | yes | yes |
| `pause` | yes | **no** |
| `resume` | yes | **no** |
| `fork` | yes | **no** |

Save and restore are macOS 14 APIs. A pause that frees memory needs the save. So on macOS 13 all
three optional verbs are `false`, and they refuse by name, for example `provider vz does not support pause on this
host; use a server that supports pause`, the same as Sysbox. `--memory` is required on this substrate, `0` is refused by name, and the value is a hard cap,
because a VM's memory is real memory on a laptop. Past the cap the whole sandbox dies and `Status`
says `OOMKilled`, as on Linux. `shard-init` bounds the guest under a cgroup 32 MB short of the VM's
memory. It reports the kill and holds the guest until the host has the marker on disk and says
stop. `docs/provider.md` has the mechanism. The smallest `--memory` is 128 MiB, and `create`
refuses less by name.

## What the spike proved

The spike was a throwaway Go program over Code-Hex/vz v3.7.1, ad-hoc signed with the virtualization
entitlement. It booted the Alpine 3.22 arm64 `virt` kernel (unwrapped to a raw Image) and
initramfs, with 1 cpu and 512 MB. The VM had a virtio console on a pipe pair, an entropy device and
a vsock device, and on the last runs a file-handle network device. It ran on 2026-09-19 on a
MacBook (M-series, macOS 14.6.1) and on `nairiclaw` (Mac mini M2, macOS 26.6), with the same
results:

1. The VM boots to the initramfs shell in about 2 s, and the guest answers on the console.
2. Pause and save take about 280 ms and write 24 MB of state for a 512 MB VM. The source resumes
   after the save and answers.
3. A restore into a second VM while the source runs takes about 200 ms, and the copy answers.
4. **One saved state restores any number of times.** Three processes restored the same file at once
   while the source still ran, and each copy answered. A fourth restore after the source stopped
   still worked. The file is not consumed. Fork is therefore one save and N restores, and the
   `Capabilities.Fork` flag is `true` on macOS 14 and later.
5. The two copies diverge. A file written in one is not in the other.
6. A file-handle network device survives save and restore. The restored guest brings `eth0` up, and
   its frames arrive on the host end of the socketpair. A restore with the device attached takes
   about 1.2 s instead of 200 ms.
7. **A process runs at most two VMs.** The third VM in one process fails to start, whether the
   identifiers are the same or distinct, and whether the VMs are restored or cold-booted. Eight VMs
   over four processes run together.
8. A restore with a fresh machine identifier is refused with `Code=12, invalid argument`.
9. **A restore needs an unlocked login session.** The helper unwraps the saved state with a key from
   the Secure Enclave, and a locked screen withholds that key. Every restore then fails with `Code=12,
   permission denied`, while a save still succeeds (SHARD-213, macOS 14.6). A headless box that
   never locks is fine. The driver test skips when `ioreg` reports the session locked.
10. **On macOS 26 a guest write to `/dev/console` never reaches the host.** The console file that
    the shim reads stays empty, while the same guest's writes to `/dev/hvc0` arrive. On macOS 14
    both arrive. Every guest-side probe and `shard-init` therefore write the virtio console by its
    own node, `hvc0`.

The spike did not prove a restore over a virtio-blk disk (SHARD-215), the vsock streams end to end
(SHARD-216), the netstack (SHARD-217), or any of this on macOS 13. `vzvm_integration_test.go` now
proves the first three on real VMs.

## What hypeman contributes

`kernel/hypeman` (MIT, copyright 2025 Kernel) runs this substrate in production, as
`lib/hypervisor/vz` and `cmd/vz-shim`. SHARD-231 read every file of both at `df4d1a56` (2026-09-18)
and decided per file what shard takes. The rule is to copy with attribution and never to import. No
shard package depends on a hypeman package, and `NOTICE` carries their MIT text. Each origin commit
below is the last commit that touched the file at that revision, so a diff against it shows what
shard changed.

### Taken, as a copy adapted to shard

| Piece | File in hypeman | Origin | Lands in |
|---|---|---|---|
| The device assembly: boot loader, console on file handles, entropy, virtio-blk, vsock, `Validate` before `NewVirtualMachine` | `cmd/vz-shim/vm.go` | `8331138c` | `pkg/vz/machine_darwin.go` (SHARD-213) |
| The machine identifier as base64 data, generated once and handed back so the caller persists it | `cmd/vz-shim/vm.go` `configurePlatform` | `8331138c` | `pkg/vz/machine_darwin.go` (SHARD-213) |
| The shim process shape: config as JSON on argv, restore-or-start, a state watcher that ends the process on `Stopped` or `Error`, and a SIGTERM that stops the VM | `cmd/vz-shim/main.go` | `561e34fd` | `cmd/shard-vz-shim` (SHARD-213) |
| The save and restore split: a `darwin && arm64` build calls the framework, and every other build returns the unsupported error | `cmd/vz-shim/save_restore_arm64.go`, `save_restore_unsupported.go` | `561e34fd` | `pkg/vz` (SHARD-213) |
| The host probe reads `kern.osproductversion`. Major 14 or later means save and restore exist, and an unparsable version means they do not | `lib/hypervisor/vz/save_restore_support.go`, `save_restore_support_darwin.go` | `5d9eff09` | `pkg/vz` capability probe (SHARD-213) |
| The shim as an embedded binary: `go:embed` the built shim and the plist, write each to a file, and run `codesign --sign - --entitlements` at first use | `lib/hypervisor/vz/starter.go` `extractShim`, `vz_shim_binary.go`, `vz_entitlements.go` | `840d6235`, `1c69f414` | SHARD-214 |
| The shim start: `Setpgid`, `Wait` in a goroutine, poll the socket, and on a timeout read the shim's log and say whether it exited early | `lib/hypervisor/vz/starter.go` `startShim`, `waitForShim` | `840d6235` | `services/provider/vzvm` (SHARD-218) |
| The vsock proxy over the shim socket: the client writes `CONNECT <port>\n`, the shim calls `SocketDevices()[0].Connect(port)`, answers `OK <port>\n` and copies both ways. The client keeps its `bufio.Reader` on the connection | `cmd/vz-shim/server.go` `handleVsockConnection`, `lib/hypervisor/vz/vsock.go` | `75c32892`, `1c69f414` | shim socket and `pkg/vz` client (SHARD-216) |

The entitlements plist is copied with one key, `com.apple.security.virtualization`. hypeman's plist
also grants `network.server` and `network.client`. Those are App Sandbox keys, and a signed command
line tool outside the sandbox does not need them, as the spike confirmed.

### Written by shard, informed by hypeman

| Piece | What hypeman has | Why shard writes its own |
|---|---|---|
| The shim config | `shimconfig/config.go` (`8331138c`): disks, NAT nets, balloon, Rosetta, the manifest of a saved VM | shard's shim has one disk, one file-handle net device and no balloon, and the identifier lives in its record instead of a manifest |
| The shim control channel | `cmd/vz-shim/server.go`, `lib/hypervisor/vz/client.go`: an HTTP API in the shape of cloud-hypervisor's | shard needs to pass the network fd over the socket (`SCM_RIGHTS`), and HTTP cannot carry it. The verbs are state, pause, resume, save, stop, vsock connect and network, each one a length-prefixed request |
| The cpu and memory bounds | `cmd/vz-shim/vm.go` (`8331138c`) `computeMemorySize`, `computeCPUCount`: clamp a request into the framework's `MinimumAllowed`/`MaximumAllowed` | shard's `--memory` and `--vcpus` are hard bounds that the record and `inspect` report, and a clamp would hand the VM more than the record says. Zero cpus means every host CPU the framework allows (SHARD-249). An explicit value outside the framework's range is refused with an error that names the range, as gVisor refuses a request below its minimum. The shim ticket (SHARD-213) ships the boundary tests at both ends of the range |
| Fork | `lib/hypervisor/vz/fork.go` (`561e34fd`): rewrite the paths in the manifest of the saved VM for the target | shard takes an APFS clone of the checkpoint's disk and restores from the record. The lesson it takes is that the device configuration, the network device included, must not change between save and restore, or the restore fails with `Code=12` |
| The unit tests | `save_restore_support_test.go`, `fork_test.go` (`5d9eff09`, `561e34fd`): the host probe matrix and the manifest path rewrites, against their registry | shard writes its own cases for every adapted behavior, next to the code that lands. SHARD-213 tests that the host probe fails closed on every row of the matrix (a non-darwin `GOOS`, a non-arm64 arch, macOS 13, an empty version, a malformed version). SHARD-215 tests the checkpoint disk pairing and the re-address message, and SHARD-216 tests the vsock handshake. The conformance suite covers the verbs and not these internals. Without the probe cases, SHARD-213 could advertise a verb the host lacks and no focused test would fail |

### Not taken

- The NAT network device (`vm.go` `createNATNetworkDevice`, `assignMACAddress`). The frames go to
  a netstack in the daemon instead (gotcha 5).
- The memory balloon, because shard's `--memory` is a hard cap and a balloon would make the VM's
  footprint a moving target on a laptop.
- `lib/instances`, the API, the guest agent and the OpenTelemetry client, because shard has its own
  record, API and `shard-init`.
- The Rosetta share (`cmd/vz-shim/rosetta_arm64.go`, `shimconfig/rosetta.go`, `8331138c`) is the
  reference for SHARD-233, which is not approved. Nothing is copied until it is.
- The test files verbatim, because they test hypeman's registry. The behaviors they prove get
  shard-owned cases, per the table above.

## Order

SHARD-212 (this page) -> 231 -> 232 -> 213 -> 214 -> 215, 216, 217 -> 218 -> 235 -> 234 -> 219.
