`make build-shard-vz-shim` puts `shard-vz-shim` here, and `pkg/vzshim` embeds this directory into
the daemon. The binary is ignored by git and `make clean` removes it. `vzshim.Install`, which the
vz provider calls before its first boot (SHARD-218), writes it to the shard root and ad-hoc signs
it there with `entitlements.plist` (`docs/macos-signing.md`). The shim itself imports `pkg/vz` and
never this package, so a rebuild never embeds the previous build.
