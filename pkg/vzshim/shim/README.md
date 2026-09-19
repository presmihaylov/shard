`make build-shard-vz-shim` puts `shard-vz-shim` here, and `pkg/vzshim` embeds this directory into
the daemon. The binary is ignored by git; `vzshim.Install` writes it to the shard root and ad-hoc
signs it there with `entitlements.plist` (`docs/macos-signing.md`). The shim itself imports
`pkg/vz` and never this package, so a rebuild never embeds the previous build.
