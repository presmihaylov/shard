# The CLI contract

This is the final shape of every verb, flag and output of `shard`. The SDKs build against it. A verb,
flag or format that holds its final shape before its work lands answers `not implemented yet`, and
the ticket that fills it in is named beside it.

## Exit codes

| code | when |
| --- | --- |
| 0 | the verb did what it says |
| 1 | any failure of shard, or of the request, with `shard: <reason>` on stderr |
| 3 | the verb, flag or format is not implemented yet |
| the command's code | `exec`, and `run` with the app's last code |
| 126, 127 | `exec` of a command that is not executable, or not found |
| 1 or more | `exec` whose output was lost, so a lost stream never reads as a pass |
| 128 + n | `run` of an app that signal n ended |
| 125 | `run` when shard itself fails, so a script tells it from an app that exits 1 |
| 130 | `run` that Ctrl+C left; the sandbox stays running |

`daemon status` exits 1 when a background task is in backoff, after it prints the whole status.

## Not implemented yet

A CLI stub parses its flags and arguments first, and a bad usage fails with 1 as it will once the
work lands. A valid call then writes nothing to stdout, writes one line to stderr, and exits 3:

```
$ shard list --format json
shard: list --format json: not implemented yet
$ echo $?
3
```

The line names the full verb path, and the flag and the format when they are what is missing. A
stub never dials the daemon.

## Global flags

They go before the verb.

| flag | what |
| --- | --- |
| `--root <dir>` | where shard keeps its state (default `/var/lib/shard`) |
| `--remote <url>` | the https URL of the proxy in front of `shard serve`; also `SHARD_REMOTE` |
| `--token-file <path>` | a token file for `--remote`; the token comes from it, then `SHARD_API_KEY`, then `SHARD_TOKEN_FILE` |
| `--ca-file <pem>` | the CA that signed the proxy's certificate; also `SHARD_CA_FILE` |
| `--version` | print the client version; it never fails |

## Names and aliases

A verb has one name. `list` and `remove` take `ls` and `rm` as aliases, at the top level and under
`image`, `secret`, `policy`, `snapshot` and `tokens` (`list` only). An alias runs the same code and
prints the same help. The help never lists an alias.

`exec` takes `-i` and `--interactive`, `-t` and `--tty`, and `-it` for both. `logs` takes `-f` and
`--follow`. `run` takes `-d` and `--detach`.

## Verbs

`shard <verb> --help` prints the flags and an example. The format column is the default `--format`;
a dash is a verb with no `--format`.

### Sandboxes

| verb | flags | format | stdout |
| --- | --- | --- | --- |
| `create <image>` | `--name --env --secret --policy --workdir --user --memory --cpus --disk` | - | the id |
| `create --snapshot <ref>` | the same, in place of the image | - | the id |
| `run <image> <command>...` | the `create` flags, `--restart --restart-retries --restart-backoff -d/--detach` | - | the app's output, or the id with `--detach` |
| `exec <ref> <argv>...` | `-i/--interactive -t/--tty --env --workdir --user` | - | the command's output |
| `list` | `--all --format` | table | the sandboxes |
| `logs <ref>` | `-f/--follow --egress` | - | the entrypoint's output, or the egress decisions |
| `inspect <ref>` | `--format` | json | the record |
| `stop <ref>` | | - | the id |
| `start <ref>` | | - | the id |
| `remove <ref>` | `--force` | - | the id |
| `pause <ref>` | | - | the id |
| `resume <ref>` | | - | the id |
| `fork <ref>` | `--name` | - | the new id |
| `cp <src> <ref>:<path>`, `cp <ref>:<path> <dst>` | `--user` | - | nothing |

`<ref>` is a sandbox id or its `--name`. A verb that prints the id prints the id even when it was given the name.

**An exec that a pause keeps from starting exits 1** with this line on stderr, and runs nothing:

```
shard: sandbox <id> is paused: resume it with shard resume <id>
```

### Images, snapshots, secrets and egress

| verb | flags | format | stdout |
| --- | --- | --- | --- |
| `pull <image>` | | - | the reference and the digest |
| `image list` | `--format` | table | the images |
| `image remove <image>` | `--force` | - | the reference |
| `image prune` | | - | each reference it removed |
| `snapshot create <ref>` | `--name` | - | the snapshot id |
| `snapshot list` | `--format` | table | the snapshots |
| `snapshot inspect <snap>` | `--format` | json | the record |
| `snapshot remove <snap>` | | - | the `<snap>` it was given |
| `secret set <NAME> [VALUE]` | `--to --placeholder` | - | the name |
| `secret list` | `--format` | table | the secrets, never a value |
| `secret remove <NAME>` | `--force` | - | the name |
| `secret grant <ref> <NAME>` | | - | the sandbox id |
| `secret ungrant <ref> <NAME>` | | - | the sandbox id |
| `policy create <name>` | `--allow --deny` | - | the name |
| `policy show <name>` | `--format` | json | the policy and its holders |
| `policy list` | `--format` | table | the policies |
| `policy remove <name>` | | - | the name |
| `policy attach <ref> <policy>` | | - | the sandbox id |
| `policy detach <ref>` | | - | the sandbox id |

`<snap>` is a snapshot id or its `--name`.

### Host and access

| verb | flags | format | stdout |
| --- | --- | --- | --- |
| `daemon` | `--provider --timeout --insecure-registry --log` | - | its log; `--log` on a Mac sends stdout and stderr to that file |
| `daemon status` | `--format` | table | the daemon's state and its tasks |
| `info` | `--format` | table | the provider a daemon would pick, and why |
| `serve` | `--listen --signing-key-file --tokens-file` | - | its log |
| `tokens mint` | `--name --signing-key-file --duration --scopes --tokens-file --format` | json | the token record |
| `tokens list` | `--signing-key-file --tokens-file --format` | table | the ledger |
| `tokens revoke <id>` | `--name --signing-key-file --tokens-file` | - | `revoked token <id>`, or `revoked <n> tokens of <sub>` with `--name` |
| `version` | `--format` | table | the client and daemon versions |

## Output formats

`--format` takes `json` or `table`, and any other word is a usage error. Naming the default changes
nothing: `list --format table` prints what `list` prints.

The format each verb does not default to is not implemented yet (SHARD-467): `--format json` on
`list`, `image list`, `snapshot list`, `secret list`, `policy list`, `tokens list`, `info`, `daemon
status` and `version`, and `--format table` on `inspect`, `snapshot inspect`, `policy show` and
`tokens mint`.

**A `--format json` call writes one value or nothing.** JSON is one value, indented by two spaces, and a list verb
prints an array. A failure to parse, to reach the daemon, or to encode writes nothing to stdout. A
list with warnings, or a `daemon status` with a task in backoff, still writes the whole value, then
the warnings or the error on stderr, and exits 1. `version --format json` with no daemon writes
nothing and fails.

### JSON

The values are synthetic.

`list` prints an array of sandbox records. `id`, `image`, `provider`, `state`, `pid`, `netns_path`,
`address`, `host_interface`, `resources` and `created_at` are always present:

```json
[
  {
    "id": "misty-otter-81c0",
    "name": "web",
    "image": "python:3.12",
    "provider": "gvisor",
    "state": "running",
    "pid": 41207,
    "netns_path": "/var/run/netns/misty-otter-81c0",
    "address": "10.87.0.2/16",
    "host_interface": "shardv2",
    "resources": {"memory_mib": 512, "vcpus": 1, "disk_mib": 2048},
    "command": ["python", "-m", "http.server"],
    "restart": {"policy": "on-failure", "retries": 3, "backoff": 1, "count": 0, "gave_up": false, "ended": false},
    "secrets": ["API_TOKEN"],
    "policy": "api-only",
    "started_at": "2026-10-01T09:31:00Z",
    "created_at": "2026-10-01T09:30:00Z"
  }
]
```

`state` is `pending`, `created`, `running`, `paused`, `unresponsive`, `stopped` or `failed`. `pid` is
0 when nothing runs. `resources` holds `memory_mib`, `vcpus` and `disk_mib`. Every other field is
absent when empty:

| field | present when |
| --- | --- |
| `name` | the sandbox has a `--name` |
| `kernel` | a microVM provider booted it; the guest kernel release tag |
| `exit_status` | the entrypoint exited at least once: `{"code": 0, "signal": 0}` |
| `exit_channel` | the daemon no longer reads the entrypoint exit from the guest, and why |
| `stopped_reason` | shard stopped it with no operator, or `shard-init` died on a stop |
| `failed_reason` | `state` is `failed` |
| `unresponsive_reason` | `state` is `unresponsive` |
| `digest` | the substrate created it; the image digest |
| `snapshot` | `create --snapshot` made it; the snapshot id |
| `checkpoint` | a pause wrote a checkpoint; the directory |
| `pausing` | `true`, during a pause |
| `command` | `run` made it; the app's argv |
| `restart` | it has a restart policy |
| `secrets` | it holds a placeholder; the secret names |
| `policy` | it holds a policy |
| `started_at` | the daemon started it at least once |

`restart.policy` is `no`, `on-failure` or `always`. `retries` is absent for no cap, `backoff` is the
first wait in seconds, `count` is the starts again on this run, and `last_at` is absent before the
first one.

`inspect` prints one sandbox record, and adds `egress` when the record names a policy:

```json
{
  "id": "misty-otter-81c0",
  "image": "python:3.12",
  "provider": "gvisor",
  "state": "stopped",
  "exit_status": {"code": 0, "signal": 0},
  "pid": 0,
  "netns_path": "/var/run/netns/misty-otter-81c0",
  "address": "10.87.0.2/16",
  "host_interface": "shardv2",
  "resources": {"memory_mib": 512, "vcpus": 1, "disk_mib": 2048},
  "policy": "api-only",
  "created_at": "2026-10-01T09:30:00Z",
  "egress": {
    "policy": "api-only",
    "rules": [
      {"action": "allow", "destination": {"kind": "cidr", "value": "10.87.0.1"}, "protocol": "udp", "ports": [53], "id": "1", "implied": "dns"},
      {"action": "allow", "destination": {"kind": "cidr", "value": "10.87.0.1"}, "protocol": "tcp", "ports": [53], "id": "2", "implied": "dns"},
      {"action": "allow", "destination": {"kind": "domain", "value": "api.example.com"}, "protocol": "tcp", "ports": [80, 443], "id": "3"}
    ]
  }
}
```

`egress.rules` is the order the host and the proxy enforce. `id` is the place of a rule in it, from
`"1"`. `action` is `allow` or `deny`, and `destination.kind` is `cidr`, `domain`, `domain-suffix` or
`group`. `protocol` and `ports` are absent for a rule over every protocol. `implied` is present on a
rule the policy did not write: `dns` when a name rule opened DNS, `dns rule` when a `dns` rule did.
When the store no longer holds the policy, `egress` is `{"policy": "<name>", "missing": true, "rules":
null}` and the sandbox reaches nothing.

`snapshot list` prints an array of snapshot records, below.

`image list`. `digest` is the full digest, `size` is bytes, and `broken` is absent when empty:

```json
[
  {
    "reference": "python:3.12",
    "digest": "sha256:0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0",
    "size": 52428800,
    "created": "2026-10-01T09:30:00Z"
  }
]
```

`secret list`, never with a value:

```json
[
  {
    "name": "API_TOKEN",
    "destinations": ["api.example.com"],
    "placeholder": "mock-API_TOKEN",
    "updated_at": "2026-10-01T09:30:00Z"
  }
]
```

`policy list`:

```json
[{"name": "api-only", "rule_count": 2}]
```

`tokens list`. `expires_at` is null for a token that never expires, and `status` is `active`,
`revoked` or `expired`:

```json
[
  {
    "id": "0123456789abcdef",
    "name": "build-agent",
    "issued_at": "2026-10-01T09:30:00Z",
    "expires_at": null,
    "scopes": ["sandbox:read", "exec"],
    "status": "active"
  }
]
```

`info`. `unreadable` is absent when the root holds no record shard cannot read:

```json
{"provider": "gvisor", "reason": "no /dev/kvm on this host"}
```

`daemon status` is the body of `GET /v0/daemon`:

```json
{
  "version": "v0.1.0",
  "pid": 4242,
  "started_at": "2026-10-01T09:30:00Z",
  "socket": "/var/lib/shard/shard.sock",
  "provider": "gvisor",
  "capabilities": {"pause": true, "resume": true, "fork": true},
  "proxy": {"plain_port": 30080, "tls_port": 30443},
  "tasks": [{"name": "proxy", "state": "running", "restarts": 0}]
}
```

`version`. `shim` is the VM shim on a Mac, and absent elsewhere:

```json
{"client": "v0.1.0", "daemon": "v0.1.0"}
```

`inspect`, `policy show` and `tokens mint` print JSON today: the sandbox record, `{name, rules, dns,
holders}`, and `{token, expires_at, scopes}` on one line.

### Tables

`list`, `image list`, `secret list`, `policy list`, `tokens list`, `info`, `daemon status` and
`version` print tables today. `list` prints `ID NAME IMAGE STATE UPTIME IP RESTART POLICY`, and
`snapshot list` prints `ID NAME SOURCE IMAGE SIZE CREATED`.

The tables of the JSON verbs:

- `inspect` prints `FIELD VALUE` rows, then `egress.policy`. A sandbox with a policy then gets an
  `ID RULE IMPLIED` section, with each rule as `policy create` takes it, for example
  `allow suffix:example.com tcp:443`.
- `snapshot inspect` prints `FIELD VALUE` rows.
- `policy show` prints `name`, `dns` and `holders`, then a `RULE` section. `holders` has no default.
- `tokens mint` prints `TOKEN EXPIRES SCOPES`.

## Snapshots

A snapshot is the files a stopped sandbox kept, with no memory image. It has an id and an optional
name, unique among snapshots, and it outlives its source.

```
shard stop web
shard snapshot create --name web-base web
shard create --name web-2 --snapshot web-base
shard snapshot list
shard snapshot inspect web-base
shard snapshot remove web-base
```

`snapshot create` refuses a running or paused sandbox. `create --snapshot` takes no image and
never pulls: the image must be on the host at the digest the snapshot recorded, and only the
provider that made the snapshot starts it. With no `--memory` or `--disk`, the new sandbox takes the
bounds its source ran under, and Firecracker and `vz` refuse a `--disk` that differs.

| route | body | answer | scope |
| --- | --- | --- | --- |
| `POST /v0/snapshots` | `{"sandbox", "name"}` | 201, the record | `sandbox:write` |
| `GET /v0/snapshots` | | 200, `{"snapshots": [...], "next"}` | `sandbox:read` |
| `GET /v0/snapshots/{ref}` | | 200, the record | `sandbox:read` |
| `DELETE /v0/snapshots/{ref}` | | 204 | `sandbox:delete` |
| `POST /v0/sandboxes` | `"snapshot"` in place of `"image"` | 201, the sandbox record | `sandbox:write` |

`sandbox` and `{ref}` take an id or a name. A create from
a snapshot takes the rest of the create body, with `resources` as
`{"memory_mib", "vcpus", "disk_mib"}`, and no `command` and no `restart`, because the new sandbox
runs `shard-init` alone.

The snapshot record:

```json
{
  "id": "quiet-heron-4f2a",
  "name": "web-base",
  "source": "misty-otter-81c0",
  "source_name": "web",
  "image": "python:3.12",
  "digest": "sha256:0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0",
  "provider": "gvisor",
  "disk_mib": 1024,
  "memory_mib": 512,
  "size": 7340032,
  "created_at": "2026-10-01T09:30:00Z"
}
```

`name` and `source_name` are absent when empty. `disk_mib` and `memory_mib` are the bounds the
source ran under, and `size` is the bytes the copy holds on the host. The client methods are
`CreateSnapshot`, `ListSnapshots`, `InspectSnapshot` and `RemoveSnapshot`.

`shard clone` and `POST /v0/sandboxes/{id}/clone` are gone. A snapshot and `create --snapshot` take
their place.
