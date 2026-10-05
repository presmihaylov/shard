"""The shared suite.

`uv run python -m suite` runs the checks of `sdks/suite/checks.txt` against `SHARD_REMOTE`, with
`SHARD_API_KEY` and `SHARD_SUITE_WILDCARD_KEY` each a `"*"` token. `SHARD_SUITE_ONLY=name,name` runs a subset,
`SHARD_SUITE_MODE=sync|async` runs one mode, and `SHARD_SUITE_IMAGE` picks the image.
"""

from __future__ import annotations

import asyncio
import os
import sys
from pathlib import Path

from ._async import runner as async_runner
from ._shared import describe
from ._sync import runner as sync_runner

LISTED = Path(__file__).resolve().parents[2] / "suite" / "checks.txt"
MODES = ("sync", "async")


class Refusal(Exception):
    """A run the suite will not start, which it reports without a stack."""


def main() -> int:
    for key in ("SHARD_REMOTE", "SHARD_API_KEY"):
        if not os.environ.get(key):
            raise Refusal(f"{key} is not set")
    registered = [check.name for check in sync_runner.CHECKS]
    if [check.name for check in async_runner.CHECKS] != registered:
        raise Refusal("the sync checks differ from the async ones: run make sdk-py")
    match_names(registered)
    names = select(registered, os.environ.get("SHARD_SUITE_ONLY", ""))
    failed = False
    for mode in modes(os.environ.get("SHARD_SUITE_MODE", "")):
        print(f"suite: {mode}", file=sys.stderr, flush=True)
        tally = sync_runner.run(names) if mode == "sync" else asyncio.run(async_runner.run(names))
        print(tally.summary(), file=sys.stderr, flush=True)
        failed = failed or tally.failed > 0
    return 1 if failed else 0


def match_names(registered: list[str]) -> None:
    """Refuse a registry that drifted from the shared list, so both SDK suites always run the same checks."""
    listed = [line for line in LISTED.read_text().split("\n") if line]
    missing = [name for name in listed if name not in registered]
    extra = [name for name in registered if name not in listed]
    if missing or extra:
        raise Refusal(
            f"the checks differ from sdks/suite/checks.txt: missing [{', '.join(missing)}], extra [{', '.join(extra)}]"
        )
    for at, (want, got) in enumerate(zip(listed, registered, strict=True)):
        if want != got:
            raise Refusal(
                f"the checks run out of the order of sdks/suite/checks.txt:"
                f" line {at + 1} is {want}, the suite has {got}"
            )


def select(registered: list[str], only: str) -> list[str]:
    wanted = [name.strip() for name in only.split(",") if name.strip()]
    if not wanted:
        return registered
    unknown = [name for name in wanted if name not in registered]
    if unknown:
        raise Refusal(f"SHARD_SUITE_ONLY names no such check: {', '.join(unknown)}")
    return [name for name in registered if name in wanted]


def modes(only: str) -> tuple[str, ...]:
    if not only:
        return MODES
    if only not in MODES:
        raise Refusal(f"SHARD_SUITE_MODE is {only!r}, not sync or async")
    return (only,)


if __name__ == "__main__":
    try:
        code = main()
    except Refusal as e:
        print(f"suite: {e}", file=sys.stderr)
        code = 1
    except Exception as e:
        print(f"suite: {describe(e)}", file=sys.stderr)
        code = 1
    sys.exit(code)
