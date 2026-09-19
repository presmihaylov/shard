`make build-shard-vz-shim` puts `shard-vz-shim` here, and `pkg/vz` embeds this directory into the
daemon. The binary is ignored by git; `InstallShim` writes it to the shard root and ad-hoc signs it
there with `entitlements.plist` (`docs/macos-signing.md`).
