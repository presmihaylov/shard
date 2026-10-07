---
"useshards": patch
"useshards-python": patch
---

A create or run takes a swap file size, as swapMiB in TypeScript and swap_mib in Python, and a sandbox record's resources name it. Left out, the daemon picks 2048 MiB on firecracker and vz and none on gvisor, runc and sysbox. `capabilities()` gains `swap`, true where a swap above 0 is allowed.
