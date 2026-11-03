---
"useshards": patch
"useshards-python": patch
---

A sandbox links other sandboxes: `sandbox.links.add(target)`, `list()` and `remove(target)`, and `create({ links })` (`links=` in Python). Each `Link` names its target and the address the sandbox reaches it at. Adding a link, or creating a sandbox with links, needs the `ssh` scope.
