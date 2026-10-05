# Releases, and what runs where

`shard` is built and tested in three places, because no single machine can run all of it.

| Where | What | Why there |
|---|---|---|
| GitHub Linux runner (`ci.yml`) | `go build`, `go vet`, `go test`, the linux cross-compile, `golangci-lint`, `govulncheck` | Every PR. The Linux build has no cgo, so it runs anywhere. |
| GitHub macOS runner (`ci.yml`, `darwin` job) | `make build-darwin` for arm64 and amd64, `go vet` and `go test` with cgo | Every PR. The Virtualization.framework binding is cgo over an ObjC framework, so the darwin binaries only build on a Mac (`docs/provider-vz.md`). |
| A Mac with bare metal, by hand | The VZ boot tests and the vz conformance suite | The runner is itself a VM with no nested virtualization, so every test that boots a VM skips there. |
| The devbox, by hand | `make itest`, `make devbox-e2e` | gVisor, Sysbox and runc need a Linux box with root. |
| devbox-shard2, by hand | The HTTPS endpoint the SDKs test against (`docs/https-devbox.md`) | A real certificate needs a public name on a public box. |

CI calls those steps directly. `make check` is the local gate before a commit, and it runs the same
checks plus the self-test of the e2e script, `scripts/e2e_test.sh`. The macOS job does for darwin
what the Linux job does for Linux. A change to `pkg/vz`, `pkg/vzshim`, `cmd/shard-vz-shim` or
`services/provider/vzvm` fails the PR when it no longer compiles or no longer passes its unit tests.

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
`shard-darwin-amd64` on a macOS runner, each with `RELEASE=1`. That builds with `-trimpath` and links
with `-s -w`, so a release binary has no symbol table, no DWARF and no build paths. A panic still
prints its stack. Both Linux binaries build with cgo off, so they link no libc and run on any
distribution, and the workflow stops if `file` does not call each one statically linked and
stripped. The workflow adds the service files from `packaging/`: `shard.service`,
`shard-serve.service`, `shard.daemon.plist` and `shard.newsyslog.conf`. An install then takes the
binaries and the units from one tag. It puts all of them, with a
`SHA256SUMS`, under a draft GitHub release named after the tag. `shard --version` reports the tag.

The notes of the draft hold three blanks: the Mac, the macOS version and the head that the VM proof
passed on. Fill them in. A release that still has the blanks in it has no proof behind it. Then
publish the draft as the latest release, and check that the install URL answers:

```
gh release edit v0.1.0 --draft=false --latest
curl -fsSIL -o /dev/null -w '%{http_code}\n' https://github.com/presmihaylov/shard/releases/latest/download/shard-darwin-arm64
```

The probe prints `200`. The install steps download from `releases/latest`, and the kernel releases
share the repo. If a kernel release is the latest one, every install URL answers `404`. `--latest`
makes the `v` tag the latest release when it is published.

The two darwin arches build one after the other on one arm64 runner. The shim and the guest init
land at the same embed path, so each `make build-darwin DARWIN_ARCH=<arch>` replaces that pair before
it builds the daemon. The amd64 binary is a cross build with `-arch x86_64`. The runner cannot run
it, so the job only checks its arch. There is no brew formula and no bottle, because SHARD-230 was
dropped. A user downloads the binary for their Mac from the release.

The guest kernel has its own workflow and its own release tag, which `docs/kernel.md` describes.

## The SDKs

The TypeScript SDK goes to npm and the Python SDK to PyPI, both as `useshards`. Each has its own
version. A release goes through one pull request, and an ordinary merge publishes nothing.

### Add a release note

A PR that changes an SDK's source (`src/`, `package.json` or `pyproject.toml`) carries a changeset: a
short note, and a patch, minor or major bump for each SDK it names. The Changesets root is `sdks/`,
where the TypeScript SDK is `useshards` and the Python SDK is `useshards-python`:

```
cd sdks
npm ci
npx changeset           # pick the SDKs, the bump, and write the note
npx changeset --empty   # a change that needs no release
```

The `sdk changeset` job in `ci.yml` fails a PR that changes an SDK without a changeset that names it.
It also fails a PR that edits an SDK version, because only the release PR changes one.

### Review the release PR

After a merge to `main` that brings changesets, the `release-pr` job of `sdk-publish.yml` opens or
updates one PR titled `Release SDKs`, from the branch `changeset-release/main`. It runs
`changeset version`, which bumps each named SDK and writes its `CHANGELOG.md`. Then
`sdks/release/release.py sync` writes the Python version into `_version.py` in PEP 440, and the
TypeScript version into its lockfile and `src/version.ts`. The PR body names each SDK, its old and
new version, its notes and its registry.

The job uses `GITHUB_TOKEN`, and a push by that token starts no workflow. So the job dispatches
`ci.yml` and `sdk-release.yml` on the branch, and their runs show on the PR's head commit. Check the
versions and the notes, wait for green, and merge.

### Publication

The merge runs `sdk-publish.yml` on `main`, one run at a time. `release.py plan` takes the top
heading of each SDK's `CHANGELOG.md` as its release record, fails when it differs from the version,
and skips an SDK whose GitHub release and registry files are both there already. For each SDK left,
`sdk-release.yml` builds the commit that set its version. It runs the shared gate (`make sdk-gate`
against a fresh runc daemon behind a TLS front), the SDK's check, the pack or build, and a clean
install and import. When the version already has a published GitHub release, the build ships that
release's files in place of its own, so the registries get the same bytes. The npm job (environment
`npm`) and the PyPI job (environment `pypi`) then publish those exact files by trusted publishing. Each job gets an OIDC token, and the repository
holds no registry token.

Last, the run tags `sdk-typescript-v<version>` or `sdk-python-v<version>`, the latter in PEP 440,
and creates its GitHub release. The releases/latest link must stay a shard binary release, so an SDK
release is never marked latest.

A prerelease such as `0.2.0-alpha.0` ships to npm under the dist-tag `alpha`, and to PyPI as
`0.2.0a0`. A stable version goes to npm under `latest`. To release prereleases, run
`npx changeset pre enter alpha` in `sdks/` in a PR. `npx changeset pre exit` ends them.

### Recover from a partial failure

Every publish step checks before it writes. A version already on its registry with the same files
is skipped, and one with different files fails the run. An existing tag or release is reused. To
retry, re-run the failed jobs, or run `gh workflow run sdk-publish.yml --ref main`. If npm took a
version and PyPI failed, the retry publishes only to PyPI. A failure on different files means the
registry holds another build of that version. A registry never replaces a version, so release a new
one with a changeset.

The first release, 0.1.0, ships the files of the GitHub releases `sdk-typescript-v0.1.0` and
`sdk-python-v0.1.0`, and keeps those tags and releases as they are.

### Setup

This is in place:

- the environments `npm` and `pypi`, each limited to the branch `main`;
- Settings > Actions > General > "Allow GitHub Actions to create and approve pull requests";
- the trusted publishers: npm and PyPI `useshards` trust `presmihaylov/shard`, the workflow
  `sdk-publish.yml`, and the environment `npm` or `pypi`.

If the `release-pr` job fails because Actions may not create a pull request, it names that setting.
