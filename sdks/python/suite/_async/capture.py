from __future__ import annotations

from useshards import AsyncSandbox, CommandResult

from .._shared import Arrival, Block, MiB, alternate, block_text, equal, expected, joined, newest, ok, writer
from .harness import AsyncContext, Check


async def run(
    sandbox: AsyncSandbox, blocks: list[Block], output_limit_bytes: int
) -> tuple[CommandResult, list[Arrival]]:
    """Run the writer with callbacks that record every chunk, and check they got every byte it wrote."""
    arrivals: list[Arrival] = []
    result = await sandbox.exec(
        writer(blocks),
        output_limit_bytes=output_limit_bytes,
        on_stdout=lambda chunk: arrivals.append(Arrival(stream="stdout", data=chunk)),
        on_stderr=lambda chunk: arrivals.append(Arrival(stream="stderr", data=chunk)),
    )
    equal(result.exit_code, 0, result.stderr[-200:])
    for stream in ("stdout", "stderr"):
        got = joined(arrivals, stream)
        ok(got == expected(blocks, stream), f"the {stream} callback got {len(got)} bytes, not every byte written")
    return result, arrivals


async def default_limit(ctx: AsyncContext) -> None:
    sandbox = await ctx.sandbox()
    blocks = [Block(stream="stdout", tag=f"d{i}", size=MiB) for i in range(9)]
    result = await sandbox.exec(writer(blocks))
    equal(len(result.stdout_bytes), 8 * MiB, "the default keeps 8 MiB")
    ok(result.stdout_bytes == b"".join(block_text(each) for each in blocks[1:]), "the default keeps the newest 8 MiB")
    equal(result.stderr_bytes, b"")


async def newest_bytes(ctx: AsyncContext) -> None:
    sandbox = await ctx.sandbox()
    limit = 8 * MiB
    result, arrivals = await run(sandbox, alternate(40, MiB // 2), limit)
    want = newest(arrivals, limit)
    equal(len(result.stdout_bytes) + len(result.stderr_bytes), limit, "stdout and stderr share one limit")
    ok(result.stdout_bytes == want["stdout"], "stdout holds its part of the newest 8 MiB")
    ok(result.stderr_bytes == want["stderr"], "stderr holds its part of the newest 8 MiB")


async def zero_disables(ctx: AsyncContext) -> None:
    sandbox = await ctx.sandbox()
    result, _ = await run(sandbox, alternate(4, MiB // 2), 0)
    equal(result.stdout_bytes, b"")
    equal(result.stderr_bytes, b"")


async def callbacks_every_chunk(ctx: AsyncContext) -> None:
    sandbox = await ctx.sandbox()
    limit = MiB
    result, arrivals = await run(sandbox, alternate(8, MiB // 2), limit)
    total = sum(len(each.data) for each in arrivals)
    equal(total - limit, 3 * MiB, "the callbacks got the 3 MiB past the limit too")
    want = newest(arrivals, limit)
    ok(
        result.stdout_bytes == want["stdout"] and result.stderr_bytes == want["stderr"],
        "the capture still keeps the newest 1 MiB",
    )


CHECKS = [
    Check("capture.default_limit", default_limit),
    Check("capture.newest_bytes", newest_bytes),
    Check("capture.zero_disables", zero_disables),
    Check("capture.callbacks_every_chunk", callbacks_every_chunk),
]
