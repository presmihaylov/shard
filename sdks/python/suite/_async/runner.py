"""One mode's run: every selected check in order, its verdict, and whatever it left behind."""

from __future__ import annotations

import sys
from collections.abc import Collection

from .._shared import Skip, Tally, drain, failure_text, first_line
from . import auth, capture, commands, files, lifecycle, logs, policies, ports, processes, secrets, snapshots
from .harness import AsyncContext, Check
from .mode import catch_strays, within

CHECKS: list[Check] = [
    *auth.CHECKS,
    *lifecycle.CHECKS,
    *snapshots.CHECKS,
    *commands.CHECKS,
    *capture.CHECKS,
    *files.CHECKS,
    *processes.CHECKS,
    *logs.CHECKS,
    *secrets.CHECKS,
    *policies.CHECKS,
    *ports.CHECKS,
]

CHECK_TIMEOUT = 300.0


async def run(names: Collection[str]) -> Tally:
    # An error nothing collected belongs to the check that was running, or to the run once the last check ended.
    strays: list[str] = []
    catch_strays(strays)
    root = AsyncContext.suite()
    tally = Tally()
    try:
        for check in CHECKS:
            if check.name not in names:
                continue
            tally.count += 1
            await verdict(root, check, strays, tally)
        leftover = [f"cleanup: {what}" for what in await root.cleanup()]
        leftover += [f"stray error: {what}" for what in drain(strays)]
        for what in leftover:
            tally.failed += 1
            print(f"FAIL suite: {what}", flush=True)
    finally:
        await root.aclose()
    return tally


async def verdict(root: AsyncContext, check: Check, strays: list[str], tally: Tally) -> None:
    """Run one check and undo what it made; a check that passed but left something behind fails."""
    ctx = root.check()
    err: Exception | None = None
    try:
        await within(CHECK_TIMEOUT, lambda: check.run(ctx))
    except Exception as e:
        err = e
    failures: list[BaseException | str] = [] if err is None or isinstance(err, Skip) else [err]
    failures += [f"cleanup: {what}" for what in await ctx.cleanup()]
    failures += [f"stray error: {what}" for what in drain(strays)]
    if failures:
        for failure in failures:
            print(f"--- {check.name}\n{failure_text(failure)}", file=sys.stderr, flush=True)
        tally.failed += 1
        print(f"FAIL {check.name}: {first_line(failures[0])}", flush=True)
        return
    if isinstance(err, Skip):
        tally.skipped += 1
        print(f"SKIP {check.name}: {err}", flush=True)
        return
    tally.passed += 1
    print(f"PASS {check.name}", flush=True)
