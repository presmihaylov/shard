---
"useshards": patch
"useshards-python": patch
---

Breaking: a sandbox runs named processes in place of one app. `sandbox.run(command, { name, restart })` starts a process that shard-init supervises and returns a `Process` with `wait()`, `logs()`, `followLogs()` (`follow_logs()` in Python) and `kill()`; `sandbox.processes.list()` and `get(name)` find them again, and the restart policy defaults to `unless-stopped`. `shard.run(image, command)`, `App`, and the sandbox's own `logs()` and `followLogs()` are gone, `create()` takes no command or restart, a sandbox record lists `processes` in place of `app`, and the error codes `no_app` and `app_ended` give way to `no_process` and `process_limit`. A run of a name that still runs answers `name_taken`.
