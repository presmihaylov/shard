# Releases, and what runs where

`shard` is built and tested in three places, because no single machine can run all of it.

| Where | What | Why there |
|---|---|---|
| GitHub Linux runner (`ci.yml`) | `go build`, `go vet`, `go test`, the linux cross-compile, `golangci-lint`, `govulncheck` | Every PR. The Linux build has no cgo, so it runs anywhere. |
| GitHub macOS runner (`ci.yml`, `darwin` job) | `make build-darwin` for arm64 and amd64, `go vet` and `go test` with cgo | Every PR. The Virtualization.framework binding is cgo over an ObjC framework, so the darwin binaries only build on a Mac (`docs/provider-vz.md`). |
| A Mac with bare metal, by hand | The VZ boot tests and the vz conformance suite | The runner is itself a VM with no nested virtualization, so every test that boots a VM skips there. |
| The devbox, by hand | `make itest`, `make devbox-e2e` | gVisor, Sysbox and runc need a Linux box with root. |

CI calls those steps directly. `make check` is the local gate before a commit, and it runs the same
checks plus the e2e script. The macOS job does for darwin what the Linux job does for Linux. A change
to `pkg/vz`, `pkg/vzshim`, `cmd/shard-vz-shim` or `services/provider/vzvm` fails the PR when it no
longer compiles or no longer passes its unit tests.

## The VM proof before a tag

The tests that the runner skips are the ones that prove a sandbox boots. They run by hand before a
release, on a Mac with the Command Line Tools and the guest kernel. None of these steps needs root.

```
make build-darwin
make kernel ARCH=arm64                       # or SHARD_KERNEL=<path> to a downloaded release kernel
CGO_ENABLED=1 go test -count=1 ./pkg/vz/...
CGO_ENABLED=1 go test -tags integration -count=1 -timeout 25m ./services/provider/vzvm/...
```

The first command matters, because the test binary carries the shim that `make build-darwin`
embedded. A stale shim boots the previous build. The save, resume and fork tests need macOS 15 or
later and an unlocked login session. On an older Mac or behind a locked screen they skip, and the
output says so. A run that reports only skips proves nothing, so read the output for `SKIP` before
you trust it.

Record the Mac, the macOS version and the head that passed in the release notes. The workflow leaves
room for them.

## Cutting a release

Push a tag of the form `v*` on `main`. `release.yml` first checks that the tagged commit is on
`main` and stops if it is not, so a tag pushed onto a feature head publishes nothing. It then builds
`shard-linux-amd64` and `shard-init-linux-amd64` on a Linux runner, and `shard-darwin-arm64` and
`shard-darwin-amd64` on a macOS runner. It puts them, with a `SHA256SUMS`, under a draft GitHub
release named after the tag. `shard --version` reports the tag. The notes of the draft hold three
blanks: the Mac, the macOS version and the head that the VM proof passed on. Fill them in and publish
the draft. A release that still has the blanks in it has no proof behind it.

The two darwin arches build one after the other on one arm64 runner. The shim and the guest init
land at the same embed path, so each `make build-darwin DARWIN_ARCH=<arch>` replaces that pair before
it builds the daemon. The amd64 binary is a cross build with `-arch x86_64`. The runner cannot run
it, so the job only checks its arch. There is no brew formula and no bottle, because SHARD-230 was
dropped. A user downloads the binary for their Mac from the release.

The guest kernel has its own workflow and its own release tag, which `docs/kernel.md` describes.
