# useshards

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
