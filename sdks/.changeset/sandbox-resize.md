---
"useshards": patch
"useshards-python": patch
---

A sandbox gains `resize()`, which changes the memory, vCPU, disk or swap bounds of a stopped sandbox, as memoryMiB, vcpus, diskMiB and swapMiB in TypeScript and memory_mib, vcpus, disk_mib and swap_mib in Python. A bound left out keeps the sandbox's own. The disk only grows. `capabilities()` gains `resize`, true on every provider, and `diskGrow` (`disk_grow` in Python), true where a resize can grow the disk.
