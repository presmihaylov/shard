# useshards

## 0.1.5

### Patch Changes

- d890cf3: Breaking: a sandbox runs named processes in place of one app. `sandbox.run(command, { name, restart })` starts a process that shard-init supervises and returns a `Process` with `wait()`, `logs()`, `followLogs()` (`follow_logs()` in Python) and `kill()`; `sandbox.processes.list()` and `get(name)` find them again, and the restart policy defaults to `unless-stopped`. `shard.run(image, command)`, `App`, and the sandbox's own `logs()` and `followLogs()` are gone, `create()` takes no command or restart, a sandbox record lists `processes` in place of `app`, and the error codes `no_app` and `app_ended` give way to `no_process` and `process_limit`. A run of a name that still runs answers `name_taken`.

## 0.1.4

### Patch Changes

- 35f836d: A sandbox record says what the host's memory kills did to it, and when the daemon starts it again, as oom in both SDKs.
- 049e976: A create or run takes a swap file size, as swapMiB in TypeScript and swap_mib in Python, and a sandbox record's resources name it. Left out, the daemon picks 2048 MiB on firecracker and vz and none on gvisor, runc and sysbox. `capabilities()` gains `swap`, true where a swap above 0 is allowed.

## 0.1.3

### Patch Changes

- d32ac0f: Forward a host port into a sandbox with `sandbox.ports.add`, `list` and `remove`, list every forward with `shard.ports()`, and forward ports from the first start with the create option `ports`. A sandbox record names its forwards, and capabilities say whether the provider forwards ports.

## 0.1.2

### Patch Changes

- 03bcd2b: A sandbox record names the sandbox it was forked from, as forkedFrom in TypeScript and forked_from in Python.

## 0.1.1

### Patch Changes

- 2939f8d: Clearer wording in the README, the docstrings and the error messages.

## 0.1.0

### Patch Changes

- 52385e8: First stable release.
