# The guest kernel

A microVM substrate boots a kernel that shard ships, and never the kernel of the host. There is one
Linux release for each shard version. It is built once per architecture, and every build of it is
byte-identical. Each microVM provider boots the kernel for the host's architecture: arm64 on vz,
and arm64 or amd64 on Firecracker (SHARD-232, shared with M8).

## What is in it

`packaging/kernel/config-<arch>` is a full `.config` with no modules and with initrd support. It
builds in virtio-blk, virtio-net, virtio-vsock, virtio-console, virtio-pci and virtio-mmio, ext4,
overlay, cgroups, namespaces and seccomp. The arm64 config also builds in squashfs; the amd64 config
disables it. The vz provider boots with an initrd that contains `shard-init`. Since build 2 the
kernel also builds in the bridge, netfilter, conntrack, NAT and nf_tables, which a runtime inside
the guest needs (SHARD-247). Since build 3 it builds in EROFS with xattrs, ACLs and LZ4, for the
read-only base disk that Firecracker mounts under its overlay (SHARD-265). Both config files started as the `ch_defconfig` of Cloud
Hypervisor at its `ch-6.12.8` tag, which hypeman boots on Virtualization.framework in production.
`olddefconfig` carries them forward to the pinned release. A change to either file means a new
`Build`.

## How it is built

```
make kernel ARCH=arm64          one build, into bin/kernel/arm64/Image-arm64 and its .sha256
make kernel ARCH=amd64          the same, bin/kernel/amd64/vmlinux-amd64
make kernel-reproducible        two builds, and a diff of the two hashes
```

The build runs in `packaging/kernel/Dockerfile`, a `debian:13` image pinned by digest. Apt points at
a dated `snapshot.debian.org` archive, and every package is pinned to an exact version. The image is
always `linux/amd64`, so a Mac and a GitHub runner produce the same bytes. Both use the same compiler
and the same cross compiler for arm64, with `KBUILD_BUILD_TIMESTAMP`, `KBUILD_BUILD_USER`,
`KBUILD_BUILD_HOST` and `SOURCE_DATE_EPOCH` fixed. The build fetches the source tarball from
`cdn.kernel.org` once into `bin/kernel/cache` and checks it against the sha256 in
`packaging/kernel/version.mk`. That file and `services/kernel.Version` must agree, and a unit test
checks that they do.

On a Mac the build runs under emulation and takes a while. Nothing in it needs a Mac, so a Linux box
with Docker builds it faster.

## How it is released and fetched

The `kernel` workflow (`.github/workflows/kernel.yml`) runs on manual dispatch only. It builds each
arch twice and compares the hashes. It checks them against the hashes that `services/kernel` was
built with, then publishes `Image-arm64`, `vmlinux-amd64` and `SHA256SUMS` under the release tag
`kernel-<version>-<build>`, titled `guest-kernel-<version>-<build>`. A hash that does not match what
the Go code expects fails the workflow, so a release can never carry a kernel that the daemon would
refuse.

When the daemon creates a microVM provider, it checks the kernel file for the host arch. On first
use, it fetches the file into `<root>/kernel/<tag>/` and fsyncs the file and its directory. At this
check, a changed release file, for example one that a host crash truncated, is fetched again. A
changed `SHARD_KERNEL` file is refused with `kernel checksum mismatch`. The daemon reuses the
provider and its kernel path for later boots, without another hash check.

A sandbox record names the kernel its guest booted in `kernel`, by release tag, for example
`kernel-6.12.110-3`. A create and a start after a stop record the kernel they boot. A resume or a
fork keeps the tag it had, because it runs a memory image of that kernel. A container substrate
leaves `kernel` empty.

### The dev path

Before the first release, or to boot a kernel that is not released, a daemon can take a kernel from
disk:

```
SHARD_KERNEL=/path/to/Image-arm64 SHARD_KERNEL_SHA256=<its sha256> shard daemon ...
```

The daemon still checks the hash, against the value given. Set both variables or neither, because
setting just one of them is an error at start. The override is meant for a developer with a fresh
build, and an install should not use it. Every microVM substrate takes the same override. On a KVM
host, `SHARD_KERNEL` names `vmlinux-amd64` for amd64 or `Image-arm64` for arm64. The Firecracker
provider boots the kernel for the host's architecture, as vz does.

A sandbox that boots a dev kernel records `local-` and the first 12 characters of its hash in
`kernel`, so two dev builds stay apart.

## Bumping it

1. Change `KERNEL_VERSION` and `KERNEL_SHA256` in `packaging/kernel/version.mk`. Take the values
   from `https://cdn.kernel.org/pub/linux/kernel/v6.x/sha256sums.asc`.
2. Set `Version` in `services/kernel/kernel.go` to the same version, and set `Build` to 1. A change
   to the config alone keeps `Version` and adds one to `Build`.
3. Run `make kernel-reproducible ARCH=arm64` and `ARCH=amd64`, then put the two hashes in
   `artifacts`.
4. Merge, then run the `kernel` workflow on `main`. It refuses if a hash differs from step 3.
