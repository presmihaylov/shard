"""One command in a sandbox: the stream that carries it, and the handle a background start answers."""

from __future__ import annotations

import time
from collections.abc import Mapping, Sequence
from typing import NoReturn

from .._capture import OutputCapture
from .._frames import OP_BINARY
from .._generated import models
from .._generated.api.exec_ import create_exec, get_exec, kill_exec, resize_exec
from .._types import CommandInfo, CommandResult, OutputCallback, Signal, TerminalSize, command_info
from .._wire import (
    EXIT,
    FAILURE,
    STDERR,
    STDIN,
    STDIN_CLOSE,
    STDOUT,
    exec_body,
    exit_of,
    failure_of,
    path,
)
from ..errors import ConflictError, ProtocolError, ShardConnectionError, ShardError
from . import _backend
from ._transport import AsyncTransport
from ._ws import CLOSE_WAIT, AsyncWebSocket

# The daemon reads at most 1 MiB in one message; smaller pieces also keep one write from holding the send lock long.
STDIN_PIECE = 64 * 1024
# A disconnect returns before the daemon lets go of its attach, so an attach right after one retries in_use this long.
_REATTACH_BOUND = 2.0
_REATTACH_PAUSE = 0.05
# The daemon's own bound: it detaches a client that reads no output for this long.
_STALL_BOUND = "30s"


class AsyncCommand:
    """A handle to one command in a sandbox, from exec(background=True) or commands.get(). Leaving its stream never ends
    it; kill() does."""

    def __init__(
        self,
        transport: AsyncTransport,
        sandbox: str,
        id: str,
        *,
        stdin: bool | None,
        output_limit_bytes: int,
        on_stdout: OutputCallback | None,
        on_stderr: OutputCallback | None,
    ) -> None:
        self.sandbox = sandbox
        self.id = id
        self._transport = transport
        self._stdin = stdin
        self._limit = output_limit_bytes
        self._on_stdout = on_stdout
        self._on_stderr = on_stderr
        self._capture = OutputCapture(output_limit_bytes)
        self._ws: AsyncWebSocket | None = None
        self._pump: _backend.Task | None = None
        self._done = _backend.Event()
        self._result: CommandResult | None = None
        self._error: Exception | None = None
        self._leaving = False
        # Held for a whole write, so two writes never interleave their pieces and a close never lands mid-write.
        self._stdin_lock = _backend.Lock()

    def __repr__(self) -> str:
        return f"{type(self).__name__}(id={self.id!r}, sandbox={self.sandbox!r})"

    @property
    def _path(self) -> str:
        return path("sandboxes", self.sandbox, "exec", self.id)

    @property
    def _what(self) -> str:
        return f"command {self.id} in sandbox {self.sandbox}"

    async def wait(self, timeout: float | None = None) -> CommandResult:
        """Block until the command ends. With no stream open it attaches, and the daemon replays the output it still
        holds."""
        if self._pump is None and self._result is None and self._error is None:
            await self._attach()
        if not await _backend.wait_event(self._done, timeout):
            raise TimeoutError(f"{self._what} did not end within {timeout}s")
        if self._error is not None:
            raise self._error
        if self._result is not None:
            return self._result
        raise ShardConnectionError(f"{self._what}: the stream closed before the command ended")

    async def kill(self, signal: Signal = "TERM") -> None:
        """Send the command one signal. This, not a disconnect or a cancellation, is what ends it."""
        body = models.KillRequest(signal=signal)
        await self._transport.send(
            lambda: kill_exec.asyncio_detailed(self.sandbox, self.id, client=self._transport.api, body=body)
        )

    async def inspect(self) -> CommandInfo:
        """Read the command again from the daemon."""
        record = await self._transport.answer(
            models.Exec, lambda: get_exec.asyncio_detailed(self.sandbox, self.id, client=self._transport.api)
        )
        return command_info(record)

    async def resize(self, rows: int, cols: int) -> None:
        """Set the terminal size of a command that runs on a terminal."""
        body = models.TerminalSize(rows=rows, cols=cols)
        await self._transport.send(
            lambda: resize_exec.asyncio_detailed(self.sandbox, self.id, client=self._transport.api, body=body)
        )

    async def write_stdin(self, data: bytes | str) -> None:
        """Send data to the command's input, in order."""
        if self._stdin is False:
            raise ValueError(f"{self._what} started without stdin=True, so it reads no input")
        async with self._stdin_lock:
            await _feed(self._attached("write_stdin"), _bytes(data))

    async def close_stdin(self) -> None:
        """End the command's input, so a command that reads to the end of its input can finish."""
        async with self._stdin_lock:
            await self._attached("close_stdin").send_binary(bytes([STDIN_CLOSE]))

    async def disconnect(self) -> None:
        """Close the command's stream and leave the command running; wait() or reconnect() attaches again."""
        ws, pump = self._ws, self._pump
        if ws is None or pump is None:
            return
        self._ws, self._pump = None, None
        self._leaving = True
        try:
            if not ws.ended:
                await ws.send_close()
        finally:
            if not await pump.join(CLOSE_WAIT):
                await pump.cancel()
                await ws.release()
                await pump.join(None)

    async def reconnect(self) -> None:
        """Attach again. The daemon replays the output it still holds, so the capture and the callbacks start over."""
        await self.disconnect()
        await self._attach()

    async def _attach(self) -> None:
        self._capture = OutputCapture(self._limit)
        self._done = _backend.Event()
        self._result, self._error, self._leaving = None, None, False
        ws = await self._open()
        self._ws = ws
        self._pump = _backend.Task(lambda: self._stream(ws))

    async def _run(self, stdin: bytes | None) -> CommandResult:
        """Stream a foreground command to its end. Leaving early lets go of the stream; the command runs on."""
        ws = await self._open()
        sender = None if stdin is None else _Sender(ws, stdin)
        try:
            result = await self._exit(ws, sender)
        except BaseException as e:
            await _abandon(ws, sender, e)
            raise
        if sender is None:
            await ws.close()
            return result
        blocked = not await sender.task.join(0)
        if blocked:
            # A daemon that stopped reading leaves the unsent input's write blocked, so only a release frees it.
            await sender.task.cancel()
            await ws.release()
            await sender.task.join(None)
        if sender.failure is not None:
            raise sender.failure
        if not blocked:
            await ws.close()
        return result

    async def _open(self) -> AsyncWebSocket:
        deadline = time.monotonic() + _REATTACH_BOUND
        while True:
            try:
                return await AsyncWebSocket.connect(self._transport.http, self._path, self._what)
            except ConflictError as e:
                if e.code != "in_use" or time.monotonic() >= deadline:
                    raise
            await _backend.sleep(_REATTACH_PAUSE)

    async def _stream(self, ws: AsyncWebSocket) -> None:
        try:
            try:
                self._result = await self._pumped(ws)
            except Exception as e:
                # wait() raises it; nothing else waits on this task.
                self._error = e
            await self._let_go(ws)
        except Exception as e:
            self._settle_close_error(e)
        finally:
            self._done.set()

    async def _let_go(self, ws: AsyncWebSocket) -> None:
        if self._result is not None and not self._leaving:
            await ws.close()
            return
        await ws.release()

    def _settle_close_error(self, e: Exception) -> None:
        if self._error is None:
            self._error = e
            return
        self._error.add_note(f"letting go of the stream also failed: {e}")

    async def _pumped(self, ws: AsyncWebSocket) -> CommandResult | None:
        """The background read: None when this side left the stream on purpose."""
        try:
            return await self._read(ws)
        except _Ended as e:
            if self._leaving:
                return None
            await self._cut(e.cause)

    async def _exit(self, ws: AsyncWebSocket, sender: _Sender | None) -> CommandResult:
        try:
            return await self._read(ws)
        except _Ended as e:
            if sender is not None and sender.failure is not None:
                raise sender.failure from e.cause
            await self._cut(e.cause)

    async def _read(self, ws: AsyncWebSocket) -> CommandResult:
        while True:
            try:
                message = await ws.receive()
            except ShardConnectionError as e:
                raise _Ended(e) from e
            if message is None:
                raise _Ended(None)
            if message.opcode != OP_BINARY or not message.payload:
                raise ProtocolError(f"the daemon sent a message on {self._what} that names no stream")
            stream, payload = message.payload[0], message.payload[1:]
            if stream in (STDOUT, STDERR):
                self._output(stream, payload)
                continue
            if stream == EXIT:
                status = exit_of(payload, self._what)
                stdout, stderr = self._capture.output()
                return CommandResult(self.id, status.code, status.signal, stdout, stderr, status.lost_bytes)
            if stream == FAILURE:
                raise failure_of(payload, self._what)
            raise ProtocolError(
                f"the daemon sent an unknown stream {stream} on {self._what}; upgrade useshards to match the daemon"
            )

    def _output(self, stream: int, data: bytes) -> None:
        self._capture.add(stream, data)
        callback = self._on_stdout if stream == STDOUT else self._on_stderr
        if callback is not None:
            callback(data)

    async def _cut(self, cause: ShardConnectionError | None) -> NoReturn:
        """Ask the record why a stream ended early, since a daemon that detaches a stalled client says nothing."""
        what = f"{self._what}: the stream ended without an exit status"
        try:
            info = await self.inspect()
        except ShardError as e:
            # The inspect is best effort, as in the CLI: a failure keeps the plain cut line.
            raise ShardConnectionError(what) from e
        raise ShardConnectionError(
            f"{what}; the daemon detaches a client that reads no output for {_STALL_BOUND}, "
            f"and the command is {info.state}, with {info.lost_bytes} bytes of output lost"
        ) from cause

    def _attached(self, verb: str) -> AsyncWebSocket:
        if self._ws is None or self._ws.ended:
            raise ShardConnectionError(f"{self._what}: {verb} needs the stream, so call reconnect() first")
        return self._ws


async def run_command(
    transport: AsyncTransport,
    sandbox: str,
    command: str | Sequence[str],
    *,
    env: Mapping[str, str] | None = None,
    workdir: str | None = None,
    user: str | None = None,
    stdin: bytes | str | None = None,
    tty: bool | TerminalSize = False,
    output_limit_bytes: int,
    on_stdout: OutputCallback | None = None,
    on_stderr: OutputCallback | None = None,
) -> CommandResult:
    data = None if stdin is None else _bytes(stdin)
    handle = await _start(
        transport,
        sandbox,
        exec_body(command, env=env, workdir=workdir, user=user, stdin=data is not None, tty=tty),
        output_limit_bytes,
        on_stdout,
        on_stderr,
    )
    return await handle._run(data)


async def start_command(
    transport: AsyncTransport,
    sandbox: str,
    command: str | Sequence[str],
    *,
    env: Mapping[str, str] | None = None,
    workdir: str | None = None,
    user: str | None = None,
    stdin: bool = False,
    tty: bool | TerminalSize = False,
    output_limit_bytes: int,
    on_stdout: OutputCallback | None = None,
    on_stderr: OutputCallback | None = None,
) -> AsyncCommand:
    handle = await _start(
        transport,
        sandbox,
        exec_body(command, env=env, workdir=workdir, user=user, stdin=stdin, tty=tty),
        output_limit_bytes,
        on_stdout,
        on_stderr,
    )
    await handle._attach()
    return handle


async def _start(
    transport: AsyncTransport,
    sandbox: str,
    body: models.ExecRequest,
    output_limit_bytes: int,
    on_stdout: OutputCallback | None,
    on_stderr: OutputCallback | None,
) -> AsyncCommand:
    # Refused before anything starts, rather than after the command already runs.
    OutputCapture(output_limit_bytes)
    record = command_info(
        await transport.answer(
            models.Exec, lambda: create_exec.asyncio_detailed(sandbox, client=transport.api, body=body)
        )
    )
    return AsyncCommand(
        transport,
        record.sandbox,
        record.id,
        # The daemon feeds a terminal whatever the client types, stdin or not.
        stdin=body.stdin is True or body.tty is True,
        output_limit_bytes=output_limit_bytes,
        on_stdout=on_stdout,
        on_stderr=on_stderr,
    )


class _Ended(Exception):
    """The stream ended before the exit; only the caller knows whether it left on purpose."""

    def __init__(self, cause: ShardConnectionError | None) -> None:
        super().__init__("the stream ended before the exit")
        self.cause = cause


class _Sender:
    """A foreground command's input, written beside the read."""

    def __init__(self, ws: AsyncWebSocket, data: bytes) -> None:
        self.ended: ShardConnectionError | None = None
        self.failure: Exception | None = None
        self._ws = ws
        self.task = _backend.Task(lambda: self._send(data))

    async def _send(self, data: bytes) -> None:
        try:
            await _feed(self._ws, data)
            await self._ws.send_binary(bytes([STDIN_CLOSE]))
        except ShardConnectionError as e:
            # @shard 2026-10-04: the stream's end refused the write, so the read's exit or cut is the outcome.
            self.ended = e
        except Exception as e:
            # Any other fault rejects the call at once, so the release wakes the read to raise it.
            self.failure = e
            try:
                await self._ws.release()
            except Exception as release_error:
                e.add_note(f"letting go of the stream also failed: {release_error}")


async def _feed(ws: AsyncWebSocket, data: bytes) -> None:
    for start in range(0, len(data), STDIN_PIECE):
        await ws.send_binary(bytes([STDIN]) + data[start : start + STDIN_PIECE])


async def _abandon(ws: AsyncWebSocket, sender: _Sender | None, cause: BaseException) -> None:
    try:
        if sender is not None:
            await sender.task.cancel()
        await ws.release()
    except Exception as e:
        cause.add_note(f"letting go of the stream also failed: {e}")
    if sender is not None and sender.ended is not None:
        cause.add_note(f"writing the command's input also failed: {sender.ended}")


def _bytes(data: bytes | str) -> bytes:
    return data.encode("utf-8") if isinstance(data, str) else bytes(data)
