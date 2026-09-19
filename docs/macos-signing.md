# Code signing on macOS

Only `shard-vz-shim` needs a signature that shard makes on purpose. Apple's Virtualization.framework
refuses to create a VM for a process without the `com.apple.security.virtualization` entitlement, and
an entitlement is carried by a code signature. The shim is the only process that touches the
framework (`docs/provider-vz.md`), so it is the only binary shard signs. The daemon and the CLI are
one Go binary, `shard`, and it stays as the Go linker leaves it.

## What ships

`make build-darwin` builds the shim, ad-hoc signs it into `pkg/vzshim/shim/`, and then builds `shard`
with the shim embedded. `pkg/vzshim` is its own package, which the daemon links and the shim does
not: a shim that embedded its own previous build would never hash the same twice. On first use the
daemon writes the shim into the shard root and ad-hoc signs it there (`vzshim.Install`), with the
entitlements plist it also embeds, in a temporary file it removes after the signature. A stamp
beside the shim, `shard-vz-shim.sha256`, holds the hash of the embedded build, so a later start
finds the shim in place, and a build with a different shim replaces the file by rename, so a
running shim keeps its inode. Concurrent first-use callers each sign a temporary copy of their
own and publish it by rename, so the path never holds a partial file. The install needs
`codesign`, which the Command Line Tools provide; Xcode is not needed.

The installed shim, on a Mac with only the Command Line Tools:

```
$ codesign -d --entitlements - /var/lib/shard/shard-vz-shim
[Dict]
	[Key] com.apple.security.virtualization
	[Value]
		[Bool] true

$ codesign -dvv /var/lib/shard/shard-vz-shim
Format=Mach-O thin (arm64)
CodeDirectory v=20400 size=56975 flags=0x2(adhoc) hashes=1769+7 location=embedded
Signature=adhoc
TeamIdentifier=not set
```

The daemon, for comparison, carries the linker's own ad-hoc signature and no entitlements. Every
arm64 Mach-O needs one to execute at all, and the Go linker adds it:

```
$ codesign -dvv bin/shard-darwin-arm64
CodeDirectory v=20400 size=122014 flags=0x20002(adhoc,linker-signed) hashes=3810+0 location=embedded
Signature=adhoc
```

## What an ad-hoc signature can and cannot do

An ad-hoc signature (`codesign --sign -`) seals the binary and its entitlements with no identity
behind it. On the machine where it was made it is enough: the kernel checks the seal, the framework
finds the entitlement, the VM boots. That is the whole of what shard needs on a developer Mac or a
self-managed Mac mini.

It cannot pass Gatekeeper on another machine as a downloaded app, it cannot be notarized, and it
carries no team identifier, so nothing can be granted to "shard" as a publisher. None of that
matters for a binary a user builds or installs from a package manager and runs from a terminal:
Gatekeeper judges quarantined downloads, not a process the user execs.

The signature is per build: the daemon re-signs the shim whenever the embedded bytes change, and a
shim copied from another Mac keeps working, since the seal does not name the machine.

## What a Developer ID adds

Signing with a Developer ID certificate (`codesign --sign "Developer ID Application: ..."`) and
notarizing the result lets a downloaded `shard` open without a Gatekeeper refusal, ties the binary to
an Apple team, and lets an MDM allow or deny shard by that team rather than by path or hash. It
changes nothing about the entitlement: `com.apple.security.virtualization` is not restricted, so
an ad-hoc and a Developer ID signature carry it the same way. A future release job can sign the
embedded shim and `shard` itself with a Developer ID; `vzshim.Install` then re-signs the extracted
shim ad-hoc, which is still valid, and a hardened build would sign with the identity instead.

## What an MDM block looks like

A managed Mac can forbid virtualization outright. The signal is not shard's: the shim starts, and
the framework refuses the VM with `VZErrorDomain Code=2` (invalid virtual machine configuration) or
the process is denied at `hv_vm_create` with `HV_DENIED`. shard reports that as the shim's exit
with its log tail, for example:

```
shard: create: start the vm: Error Domain=VZErrorDomain Code=2 "The virtual machine configuration is invalid."
```

The profile behind it is a restrictions payload with `allowVirtualMachines` (or an endpoint security
policy that blocks `com.apple.Virtualization.VirtualMachine`, the framework's helper process). There
is no workaround in shard, and there should not be one: the fix is the MDM policy, and the message
names the framework so the owner knows where to look.
