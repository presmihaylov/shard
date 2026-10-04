"""One step of a cross-SDK check, in a process of its own, so the next step shares no client state with this one."""

from __future__ import annotations

import asyncio
import hashlib
import json
import sys
import threading
from collections.abc import Callable
from pathlib import Path
from typing import Any

from useshards import AsyncShard, Shard


def create(shard: Shard, image: str, name: str) -> dict[str, Any]:
    return {"id": shard.create(image, name=name).id}


def remove(shard: Shard, id: str) -> dict[str, Any]:
    shard.get(id).remove(force=True)
    return {}


def exec_(shard: Shard, id: str, script: str) -> dict[str, Any]:
    result = shard.get(id).exec(script)
    return {"exit_code": result.exit_code, "stdout": result.stdout, "stderr": result.stderr}


def find(shard: Shard, id: str, marker: str) -> dict[str, Any]:
    """A second client finds the command another process started by its marker, and reads it to its end."""
    sandbox = shard.get(id)
    entry = next((each for each in sandbox.commands.list() if marker in " ".join(each.command)), None)
    if entry is None:
        return {"state_at_list": None}
    result = sandbox.commands.get(entry.id).wait()
    file = sandbox.files.read_text(f"/tmp/{marker}")
    return {"state_at_list": entry.state, "exit_code": result.exit_code, "stdout": result.stdout, "file": file}


def start(shard: Shard, id: str, marker: str) -> dict[str, Any]:
    """A background command this client lets go of once it printed, so another client can reconnect to it."""
    one = threading.Event()

    def on_stdout(chunk: bytes) -> None:
        if b"one" in chunk:
            one.set()

    command = shard.get(id).exec(f"echo one; sleep 4; echo {marker}", background=True, on_stdout=on_stdout)
    if not one.wait(30):
        raise RuntimeError("the background command printed no one within 30 s")
    command.disconnect()
    return {"command": command.id}


def hold(shard: Shard, id: str) -> dict[str, Any]:
    """A terminal command this client lets go of once it runs, so the gate can resize and kill it over plain HTTP."""
    held = threading.Event()

    def on_stdout(chunk: bytes) -> None:
        if b"held" in chunk:
            held.set()

    command = shard.get(id).exec("echo held; sleep 300", background=True, tty=True, on_stdout=on_stdout)
    if not held.wait(30):
        raise RuntimeError("the terminal command printed no held within 30 s")
    command.disconnect()
    return {"command": command.id}


def attach(shard: Shard, id: str, command: str) -> dict[str, Any]:
    result = shard.get(id).commands.get(command).wait()
    return {"exit_code": result.exit_code, "stdout": result.stdout}


def capture(shard: Shard, id: str, script: str, limit: str) -> dict[str, Any]:
    seen = 0

    def on_stdout(chunk: bytes) -> None:
        nonlocal seen
        seen += len(chunk)

    result = shard.get(id).exec(script, output_limit_bytes=int(limit), on_stdout=on_stdout)
    return captured(result.exit_code, seen, result.lost_bytes, result.stdout_bytes, result.stderr_bytes)


def put(shard: Shard, id: str, local: str, remote: str) -> dict[str, Any]:
    shard.get(id).files.write(remote, Path(local).read_bytes())
    return {}


def get(shard: Shard, id: str, remote: str) -> dict[str, Any]:
    data = shard.get(id).files.read(remote)
    return {"size": len(data), "sha256": hashlib.sha256(data).hexdigest()}


def captured(exit_code: int, seen: int, lost: int, stdout: bytes, stderr: bytes) -> dict[str, Any]:
    return {
        "exit_code": exit_code,
        "seen": seen,
        "lost_bytes": lost,
        "stdout_len": len(stdout),
        "stdout_sha256": hashlib.sha256(stdout).hexdigest(),
        "stderr_len": len(stderr),
    }


async def cancel(id: str, marker: str) -> dict[str, Any]:
    """A foreground exec whose task this client cancels once it runs; the marker file shows it outlived the cancel."""
    async with AsyncShard() as shard:
        sandbox = await shard.get(id)
        started = asyncio.Event()

        def on_stdout(chunk: bytes) -> None:
            if b"started" in chunk:
                started.set()

        call = asyncio.ensure_future(
            sandbox.exec(f"echo started; sleep 8; echo {marker} > /tmp/{marker}", on_stdout=on_stdout)
        )
        seen = asyncio.ensure_future(started.wait())
        await asyncio.wait({call, seen}, return_when=asyncio.FIRST_COMPLETED)
        if call.done():
            seen.cancel()
            return {"cancelled": None}
        call.cancel()
        await asyncio.wait({call})
        return {"cancelled": "CancelledError" if call.cancelled() else repr(call.exception())}


async def capture_async(id: str, script: str, limit: str) -> dict[str, Any]:
    async with AsyncShard() as shard:
        seen = 0

        def on_stdout(chunk: bytes) -> None:
            nonlocal seen
            seen += len(chunk)

        result = await (await shard.get(id)).exec(script, output_limit_bytes=int(limit), on_stdout=on_stdout)
        return captured(result.exit_code, seen, result.lost_bytes, result.stdout_bytes, result.stderr_bytes)


SYNC: dict[str, Callable[..., dict[str, Any]]] = {
    "create": create,
    "remove": remove,
    "exec": exec_,
    "find": find,
    "start": start,
    "hold": hold,
    "attach": attach,
    "capture": capture,
    "put": put,
    "get": get,
}


def main(step: str, args: list[str]) -> dict[str, Any]:
    if step == "cancel":
        return asyncio.run(cancel(*args))
    if step == "capture-async":
        return asyncio.run(capture_async(*args))
    if step not in SYNC:
        raise SystemExit(f"no step {step}; want one of {', '.join([*SYNC, 'cancel', 'capture-async'])}")
    with Shard() as shard:
        return SYNC[step](shard, *args)


if __name__ == "__main__":
    print(json.dumps(main(sys.argv[1], sys.argv[2:])), flush=True)
