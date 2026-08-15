# shard

A single-node sandbox manager. One binary runs isolated sandboxes on a Linux host, with or without
hardware virtualization, and gives them the same lifecycle verbs either way: run, exec, sleep, wake
and fork. On a host with `/dev/kvm` it drives Firecracker microVMs; on a host without one it drives
gVisor. An optional `shard serve` exposes the same verbs over a self-hostable REST API.

**Status: pre-alpha.** Nothing here works yet. The repository is a skeleton.

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
