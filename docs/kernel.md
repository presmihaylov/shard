# The guest kernel

A microVM substrate boots a kernel shard ships, never one from the host. There is one Linux release
per shard version, built once per architecture, and every build of it is byte-identical. The VZ
provider boots the arm64 one; Firecracker will boot the amd64 one (SHARD-232, shared with M8).

## What is in it

`packaging/kernel/config-<arch>` is a full `.config` with no modules and no initrd: virtio-blk,
virtio-net, virtio-vsock, virtio-console, virtio-pci and virtio-mmio, ext4, overlay, squashfs,
cgroups, namespaces and seccomp are built in. Both started as Cloud Hypervisor's `ch_defconfig` at
their `ch-6.12.8` tag, which hypeman boots on Virtualization.framework in production, and
`olddefconfig` carries them to the pinned release. A change to either file is a new `Build`.

## How it is built

```
make kernel ARCH=arm64          one build, into bin/kernel/arm64/Image-arm64 and its .sha256
make kernel ARCH=amd64          the same, bin/kernel/amd64/vmlinux-amd64
make kernel-reproducible        two builds, and a diff of the two hashes
```

The build runs in `packaging/kernel/Dockerfile`, a `debian:13` image pinned by digest and always
`linux/amd64`, so a Mac and a GitHub runner produce the same bytes: same compiler, same cross
compiler for arm64, and `KBUILD_BUILD_TIMESTAMP`, `KBUILD_BUILD_USER`, `KBUILD_BUILD_HOST` and
`SOURCE_DATE_EPOCH` fixed. The source tarball is fetched from `cdn.kernel.org` once into
`bin/kernel/cache` and checked against the sha256 in `packaging/kernel/version.mk`. That file and
`services/kernel.Version` must agree; a unit test says so.

On a Mac the build is emulated and takes a while; nothing in it needs a Mac, so a Linux box with
Docker is the faster place.

## How it is released and fetched

The `kernel` workflow (`.github/workflows/kernel.yml`, manual dispatch only) builds each arch
twice, compares the hashes, checks them against the ones `services/kernel` was built with, and
publishes `Image-arm64`, `vmlinux-amd64` and `SHA256SUMS` under the release tag `kernel-<version>-<build>`.
A hash that does not match what the Go code expects fails the workflow, so a release can never
carry a kernel the daemon would refuse.

The daemon fetches the file for the host arch into `<root>/kernel/<tag>/` on first use, hashes it
before every boot, and refuses one that changed: `kernel checksum mismatch`. `shard inspect` shows
the tag in `kernel` on a sandbox that booted one.

### The dev path

Before the first release, or to boot a kernel that is not released, a daemon takes one from disk:

```
SHARD_KERNEL=/path/to/Image-arm64 SHARD_KERNEL_SHA256=<its sha256> shard daemon ...
```

The hash is still checked, against the value given. Both variables or neither; one alone is an
error at start. This is for a developer with a fresh build, not for an install.

## Bumping it

1. Change `KERNEL_VERSION` and `KERNEL_SHA256` in `packaging/kernel/version.mk`, from
   `https://cdn.kernel.org/pub/linux/kernel/v6.x/sha256sums.asc`.
2. Set `Version` in `services/kernel/kernel.go` to the same, and `Build` to 1; a config-only
   change keeps `Version` and adds one to `Build`.
3. `make kernel-reproducible ARCH=arm64` and `ARCH=amd64`; put the two hashes in `artifacts`.
4. Merge, then run the `kernel` workflow on `main`. It refuses if a hash differs from step 3.
