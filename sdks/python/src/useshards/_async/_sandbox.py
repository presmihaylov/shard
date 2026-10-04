"""One sandbox and what runs in it: its commands, the app a run started, its logs and its files."""

from __future__ import annotations

import builtins
from collections.abc import Mapping, Sequence
from typing import Literal, overload

import httpx

from .._capture import DEFAULT_OUTPUT_LIMIT
from .._generated import models
from .._generated.api.app import attach_app, stop_app
from .._generated.api.exec_ import get_exec, list_execs
from .._generated.api.sandboxes import (
    fork_sandbox,
    get_sandbox,
    get_sandbox_egress_log,
    get_sandbox_logs,
    pause_sandbox,
    remove_sandbox,
    resume_sandbox,
    start_sandbox,
    stop_sandbox,
)
from .._generated.types import UNSET
from .._types import (
    AppExit,
    AppInfo,
    CommandInfo,
    CommandResult,
    NetworkLogRecord,
    OutputCallback,
    SandboxInfo,
    TerminalSize,
    app_exit,
    command_info,
    network_log_record,
    sandbox_info,
)
from .._wire import AsyncCall, path
from ..errors import ProtocolError, ShardConnectionError
from ._command import AsyncCommand, run_command, start_command
from ._files import AsyncFiles
from ._follow import AsyncFollow, log_chunk, network_log_entry
from ._transport import AsyncTransport


class AsyncSandbox:
    """One sandbox. info is its record as of the last call that answered one; inspect() reads it again."""

    def __init__(self, transport: AsyncTransport, info: SandboxInfo) -> None:
        self._transport = transport
        self.info = info
        self.commands = AsyncCommands(transport, info.id)
        self.files = AsyncFiles(transport, info.id)

    def __repr__(self) -> str:
        return f"{type(self).__name__}(id={self.id!r}, name={self.name!r}, state={self.info.state!r})"

    @property
    def id(self) -> str:
        return self.info.id

    @property
    def name(self) -> str | None:
        return self.info.name

    async def inspect(self) -> SandboxInfo:
        record = await self._transport.answer(
            models.Inspection, lambda: get_sandbox.asyncio_detailed(self.id, client=self._transport.api)
        )
        self.info = sandbox_info(record)
        return self.info

    @overload
    async def exec(
        self,
        command: str | Sequence[str],
        *,
        background: Literal[False] = False,
        stdin: bytes | str | None = None,
        env: Mapping[str, str] | None = None,
        workdir: str | None = None,
        user: str | None = None,
        tty: bool | TerminalSize = False,
        output_limit_bytes: int = DEFAULT_OUTPUT_LIMIT,
        on_stdout: OutputCallback | None = None,
        on_stderr: OutputCallback | None = None,
    ) -> CommandResult: ...

    @overload
    async def exec(
        self,
        command: str | Sequence[str],
        *,
        background: Literal[True],
        stdin: bool = False,
        env: Mapping[str, str] | None = None,
        workdir: str | None = None,
        user: str | None = None,
        tty: bool | TerminalSize = False,
        output_limit_bytes: int = DEFAULT_OUTPUT_LIMIT,
        on_stdout: OutputCallback | None = None,
        on_stderr: OutputCallback | None = None,
    ) -> AsyncCommand: ...

    async def exec(
        self,
        command: str | Sequence[str],
        *,
        background: bool = False,
        stdin: bytes | str | bool | None = None,
        env: Mapping[str, str] | None = None,
        workdir: str | None = None,
        user: str | None = None,
        tty: bool | TerminalSize = False,
        output_limit_bytes: int = DEFAULT_OUTPUT_LIMIT,
        on_stdout: OutputCallback | None = None,
        on_stderr: OutputCallback | None = None,
    ) -> CommandResult | AsyncCommand:
        """A cancel or a dropped connection leaves the remote command running."""
        if background:
            if stdin is not None and not isinstance(stdin, bool):
                raise TypeError("a background command takes its input through write_stdin(), so stdin is a bool")
            return await start_command(
                self._transport,
                self.id,
                command,
                env=env,
                workdir=workdir,
                user=user,
                stdin=stdin is True,
                tty=tty,
                output_limit_bytes=output_limit_bytes,
                on_stdout=on_stdout,
                on_stderr=on_stderr,
            )
        if isinstance(stdin, bool):
            raise TypeError("only a background command keeps its input open; pass the input itself instead")
        return await run_command(
            self._transport,
            self.id,
            command,
            env=env,
            workdir=workdir,
            user=user,
            stdin=stdin,
            tty=tty,
            output_limit_bytes=output_limit_bytes,
            on_stdout=on_stdout,
            on_stderr=on_stderr,
        )

    async def stop(self) -> None:
        await self._verb(lambda: stop_sandbox.asyncio_detailed(self.id, client=self._transport.api))

    async def start(self) -> None:
        await self._verb(lambda: start_sandbox.asyncio_detailed(self.id, client=self._transport.api))

    async def pause(self) -> None:
        await self._verb(lambda: pause_sandbox.asyncio_detailed(self.id, client=self._transport.api))

    async def resume(self) -> None:
        await self._verb(lambda: resume_sandbox.asyncio_detailed(self.id, client=self._transport.api))

    async def fork(self, *, name: str | None = None) -> AsyncSandbox:
        """A running copy of this sandbox, memory and all; the source runs on."""
        body = models.CopyRequest(name=name or UNSET)
        record = await self._transport.answer(
            models.Sandbox,
            lambda: fork_sandbox.asyncio_detailed(self.id, client=self._transport.api, body=body),
            self._transport.read_bound(None),
        )
        return AsyncSandbox(self._transport, sandbox_info(record))

    async def remove(self, *, force: bool = False) -> None:
        """Remove a stopped sandbox; force stops a running one first."""
        await self._transport.send(
            lambda: remove_sandbox.asyncio_detailed(self.id, client=self._transport.api, force=force or UNSET),
            self._transport.read_bound(None),
        )

    async def logs(self) -> str:
        """The app's output so far, both streams as the daemon wrote them."""
        return await self._transport.answer(
            str, lambda: get_sandbox_logs.asyncio_detailed(self.id, client=self._transport.api)
        )

    def follow_logs(self) -> AsyncFollow[bytes]:
        """The app's output from the start of the log, then as it arrives, until the sandbox stops."""
        return AsyncFollow(
            self._transport, path("sandboxes", self.id, "logs"), f"the logs of sandbox {self.id}", log_chunk
        )

    async def network_logs(self) -> builtins.list[NetworkLogRecord]:
        """Every egress decision the daemon still holds, oldest first."""
        records = await self._transport.answer(
            builtins.list, lambda: get_sandbox_egress_log.asyncio_detailed(self.id, client=self._transport.api)
        )
        return [network_log_record(record) for record in records]

    def follow_network_logs(self) -> AsyncFollow[NetworkLogRecord]:
        return AsyncFollow(
            self._transport,
            path("sandboxes", self.id, "egress-log"),
            f"the network log of sandbox {self.id}",
            network_log_entry,
        )

    async def _verb(self, call: AsyncCall) -> None:
        self.info = sandbox_info(await self._transport.answer(models.Sandbox, call, self._transport.read_bound(None)))


class AsyncCommands:
    """The commands started in one sandbox, by this client or any other."""

    def __init__(self, transport: AsyncTransport, sandbox: str) -> None:
        self._transport = transport
        self._sandbox = sandbox

    async def list(self) -> builtins.list[CommandInfo]:
        records = await self._transport.listed(
            models.ExecsResponse,
            lambda cursor: list_execs.asyncio_detailed(self._sandbox, client=self._transport.api, cursor=cursor),
            lambda page: page.execs,
        )
        return [command_info(record) for record in records]

    async def get(
        self,
        id: str,
        *,
        output_limit_bytes: int = DEFAULT_OUTPUT_LIMIT,
        on_stdout: OutputCallback | None = None,
        on_stderr: OutputCallback | None = None,
    ) -> AsyncCommand:
        """A handle on a command already started; its wait() attaches, so the callbacks see the replay."""
        record = command_info(
            await self._transport.answer(
                models.Exec, lambda: get_exec.asyncio_detailed(self._sandbox, id, client=self._transport.api)
            )
        )
        return AsyncCommand(
            self._transport,
            record.sandbox,
            record.id,
            stdin=None,
            output_limit_bytes=output_limit_bytes,
            on_stdout=on_stdout,
            on_stderr=on_stderr,
        )


class AsyncApp:
    """The app a run started. It ends on its own or by stop(); the sandbox outlives it either way."""

    def __init__(self, transport: AsyncTransport, sandbox: AsyncSandbox) -> None:
        self._transport = transport
        self.sandbox = sandbox

    async def inspect(self) -> AppInfo:
        info = await self.sandbox.inspect()
        if info.app is None:
            raise ProtocolError(f"sandbox {info.id} answered a record with no app")
        return info.app

    async def wait(self, timeout: float | None = None) -> AppExit:
        """Block until the app ends with no start again left. A timeout ends the wait, never the app."""
        try:
            record = await self._transport.answer(
                models.AppExit,
                lambda: attach_app.asyncio_detailed(self.sandbox.id, client=self._transport.api),
                self._transport.read_bound(timeout),
            )
        except ShardConnectionError as e:
            if isinstance(e.__cause__, httpx.ReadTimeout):
                raise TimeoutError(f"the app of sandbox {self.sandbox.id} did not end within {timeout}s") from None
            raise
        return app_exit(record)

    async def logs(self) -> str:
        return await self.sandbox.logs()

    async def stop(self, *, force: bool = False) -> None:
        """End the app with TERM, or KILL with force, and cancel its restart policy."""
        body = models.AppStopRequest(force=force or UNSET)
        await self._transport.send(
            lambda: stop_app.asyncio_detailed(self.sandbox.id, client=self._transport.api, body=body),
            self._transport.read_bound(None),
        )
