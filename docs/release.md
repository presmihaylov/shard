# Releases, and what runs where

`shard` is built and tested in three places, because no one machine can run all of it.

| Where | What | Why there |
|---|---|---|
| GitHub Linux runner (`ci.yml`) | `go build`, `go vet`, `go test`, the linux cross-compile, `golangci-lint`, `govulncheck` | Every PR. The Linux build has no cgo, so it runs anywhere. |
| GitHub macOS runner (`ci.yml`, `darwin` job) | `make build-darwin` for arm64 and amd64, `go vet` and `go test` with cgo | Every PR. The Virtualization.framework binding is cgo over an ObjC framework, so the darwin binaries only build on a Mac (`docs/provider-vz.md`). |
| A Mac with bare metal, by hand | The VZ boot tests and the vz conformance suite | The runner is itself a VM with no nested virtualization, so every test that boots a VM skips there. |
| The devbox, by hand | `make itest`, `make devbox-e2e` | gVisor, Sysbox and runc need a Linux box with root. |

CI calls those steps directly; `make check` is the local gate before a commit, the same checks plus
the e2e script. The macOS job is the Linux job on the other platform: a change to `pkg/vz`, `pkg/vzshim`, `cmd/shard-vz-shim` or
`services/provider/vzvm` that no longer compiles or passes its unit tests fails the PR.

## The VM proof before a tag

The tests the runner skips are the ones that prove a sandbox boots, so they run by hand before a
release, on a Mac with the Command Line Tools and the guest kernel. Nothing here needs root.

```
make build-darwin
make kernel ARCH=arm64                       # or SHARD_KERNEL=<path> to a downloaded release kernel
CGO_ENABLED=1 go test -count=1 ./pkg/vz/...
CGO_ENABLED=1 go test -tags integration -count=1 -timeout 25m ./services/provider/vzvm/...
```

The first command matters: the test binary carries the shim `make build-darwin` embedded, and a
stale one boots the previous build. The save, resume and fork tests need macOS 15 or later and an
unlocked login session; an older Mac or a locked screen skips them and says so. A run that reports
only skips has proved nothing: read the output for `SKIP` before you trust it.

Record the Mac, the macOS version and the head that passed in the release notes; the workflow leaves
room for them.

## Cutting a release

Push a tag of the form `v*` on `main`. `release.yml` first checks the tagged commit is on `main`
and stops if it is not, so a tag pushed onto a feature head publishes nothing. It then builds
`shard-linux-amd64` and `shard-init-linux-amd64` on a Linux runner, `shard-darwin-arm64` and
`shard-darwin-amd64` on a macOS runner, and puts them with a `SHA256SUMS` under a draft GitHub
release named after the tag. `shard --version` reports the tag. The draft's notes hold three blanks,
the Mac, the macOS version and the head the VM proof passed on; fill them in and publish the
draft. A release with the blanks still in it is one nobody proved.

The two darwin arches build one after the other on one arm64 runner: the shim and the guest init
land at one embed path, so each `make build-darwin DARWIN_ARCH=<arch>` replaces the pair before it
builds the daemon. The amd64 binary is a cross build with `-arch x86_64`; the runner cannot run it,
so the job checks its arch and nothing more. There is no brew formula and no bottle: SHARD-230 was
dropped, and a user downloads the binary for their Mac from the release.

The guest kernel has its own workflow and its own release tag; `docs/kernel.md` says how.
