# Code signing on macOS

Only `shard-vz-shim` needs a signature that shard makes on purpose. Apple's Virtualization.framework
refuses to create a VM for a process that lacks the `com.apple.security.virtualization` entitlement,
and only a code signature can carry an entitlement. The shim is the only process that touches the
framework (`docs/provider-vz.md`), so it is the only binary shard signs. The daemon and the CLI are
one Go binary, `shard`, and shard leaves it as the Go linker built it.

## What ships

`make build-darwin` builds the shim, ad-hoc signs it into `pkg/vzshim/shim/`, and then builds `shard`
with the shim embedded. `pkg/vzshim` is its own package, which the daemon links and the shim does
not. A shim that embedded its own previous build would never hash the same twice. `make clean`
also removes the built shim, so a plain `go build` after it embeds no shim and `Install` returns
`ErrNoShim`.

The vz provider (SHARD-218) calls `vzshim.Install` before its first boot. The install writes the
shim into the shard root and ad-hoc signs it there. The signature uses the entitlements plist, which
the package also embeds and writes to a temporary file that it removes after it signs. A stamp beside
the shim, `shard-vz-shim.sha256`, holds the hash of the embedded build and the hash of the signed
file. A later start hashes the shim and keeps it only when both hashes match, so a shim that a host
crash truncated gets signed again. A build with a different shim replaces the file by rename, so a
running shim keeps its inode. Concurrent callers each sign their own temporary copy. Each one
publishes the signed bytes through the atomic write in `pkg/store`, which fsyncs the file and its
directory, so the path never holds a partial file. The install needs `codesign`, which comes with the
Command Line Tools. It does not need Xcode.

The installed shim, on a Mac with only the Command Line Tools and `/var/lib/shard` as the root:

```
$ codesign -d --entitlements - /var/lib/shard/vz/shard-vz-shim
[Dict]
	[Key] com.apple.security.virtualization
	[Value]
		[Bool] true

$ codesign -dvv /var/lib/shard/vz/shard-vz-shim
Format=Mach-O thin (arm64)
CodeDirectory v=20400 size=56975 flags=0x2(adhoc) hashes=1769+7 location=embedded
Signature=adhoc
TeamIdentifier=not set
```

For comparison, the daemon carries the ad-hoc signature that the linker made, and no entitlements.
Every arm64 Mach-O needs a signature to execute at all, and the Go linker adds it:

```
$ codesign -dvv bin/shard-darwin-arm64
CodeDirectory v=20400 size=122014 flags=0x20002(adhoc,linker-signed) hashes=3810+0 location=embedded
Signature=adhoc
```

## What an ad-hoc signature can and cannot do

An ad-hoc signature (`codesign --sign -`) seals the binary and its entitlements, but no identity
stands behind it. That is enough on the machine where the signature was made. The kernel checks the
seal, the framework finds the entitlement, and the VM boots. On a developer Mac or a self-managed
Mac mini, shard needs nothing more.

An ad-hoc signature cannot pass Gatekeeper as a downloaded app on another machine, and it cannot be
notarized. It also carries no team identifier, so nothing can be granted to "shard" as a publisher.
None of that matters for a binary that a user builds, or installs from a package manager, and runs
from a terminal. Gatekeeper only judges quarantined downloads, and a process that the user execs is
not one.

Each build gets its own signature. The daemon re-signs the shim whenever the embedded bytes change.
A shim copied from another Mac keeps working, because the seal does not name the machine.

## What a Developer ID adds

Signing with a Developer ID certificate (`codesign --sign "Developer ID Application: ..."`) and
notarizing the result lets a downloaded `shard` open without a Gatekeeper refusal. It also ties the
binary to an Apple team, so an MDM can allow or deny shard by that team instead of by path or hash.
It changes nothing about the entitlement. `com.apple.security.virtualization` is not a restricted
entitlement, so an ad-hoc signature and a Developer ID signature carry it the same way. A future
release job can sign the embedded shim and `shard` itself with a Developer ID. `vzshim.Install` then
re-signs the extracted shim ad-hoc, and that signature is still valid. A hardened build would sign
with the identity instead.

## What an MDM block looks like

A managed Mac can forbid virtualization outright. In that case the refusal comes from macOS, and
shard does not raise it. The shim starts, and then either the framework refuses the VM with
`VZErrorDomain Code=2` (invalid virtual machine configuration), or the system denies the process at
`hv_vm_create` with `HV_DENIED`. shard reports this as the exit of the shim, with the tail of its
log, for example:

```
shard: create: start the vm: Error Domain=VZErrorDomain Code=2 "The virtual machine configuration is invalid."
```

The profile behind the block is a restrictions payload with `allowVirtualMachines`. It can also be
an endpoint security policy that blocks `com.apple.Virtualization.VirtualMachine`, which is the helper
process of the framework. shard has no workaround for this, and it should not have one, because the
fix belongs in the MDM policy. The message names the framework so that the owner knows where to look.
