"""One sandbox and what runs in it: its commands, the app a run started, its logs and its files."""

from __future__ import annotations

import builtins
from collections.abc import Mapping, Sequence
from typing import Any, Literal, overload

import httpx

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
from .._wire import json_of, path
from ..errors import ProtocolError, ShardConnectionError
from ._command import AsyncCommand, run_command, start_command
from ._files import AsyncFiles
from ._follow import AsyncFollow, log_chunk, network_log_entry
from ._transport import AsyncTransport

DEFAULT_OUTPUT_LIMIT = 8 << 20


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
        self.info = sandbox_info(json_of(await self._transport.call("GET", path("sandboxes", self.id))))
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
        """Run a command and answer how it ended, or with background=True start it and answer its handle.

        A string runs under /bin/sh -c. Foreground, stdin is the whole input; in the background, stdin=True
        keeps the input open for write_stdin(). A cancel or a dropped connection leaves the command running.
        """
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
        await self._verb("stop")

    async def start(self) -> None:
        await self._verb("start")

    async def pause(self) -> None:
        await self._verb("pause")

    async def resume(self) -> None:
        await self._verb("resume")

    async def fork(self, *, name: str | None = None) -> AsyncSandbox:
        """A running copy of this sandbox, memory and all; the source runs on."""
        response = await self._transport.call(
            "POST",
            path("sandboxes", self.id, "fork"),
            json={"name": name} if name else {},
            timeout=self._transport.read_bound(None),
        )
        return AsyncSandbox(self._transport, sandbox_info(json_of(response)))

    async def remove(self, *, force: bool = False) -> None:
        """Remove a stopped sandbox; force stops a running one first."""
        await self._transport.call(
            "DELETE",
            path("sandboxes", self.id),
            params={"force": "true"} if force else None,
            timeout=self._transport.read_bound(None),
        )

    async def logs(self) -> str:
        """The app's output so far, both streams as the daemon wrote them."""
        response = await self._transport.call("GET", path("sandboxes", self.id, "logs"))
        return response.content.decode("utf-8", "replace")

    def follow_logs(self) -> AsyncFollow[bytes]:
        """The app's output from the start of the log, then as it arrives, until the sandbox stops."""
        return AsyncFollow(
            self._transport, path("sandboxes", self.id, "logs"), f"the logs of sandbox {self.id}", log_chunk
        )

    async def network_logs(self) -> builtins.list[NetworkLogRecord]:
        """Every egress decision the daemon still holds, oldest first."""
        records = json_of(await self._transport.call("GET", path("sandboxes", self.id, "egress-log")))
        if not isinstance(records, builtins.list):
            raise ProtocolError(f"the network log of sandbox {self.id} is not a list")
        return [network_log_record(record) for record in records]

    def follow_network_logs(self) -> AsyncFollow[NetworkLogRecord]:
        return AsyncFollow(
            self._transport,
            path("sandboxes", self.id, "egress-log"),
            f"the network log of sandbox {self.id}",
            network_log_entry,
        )

    async def _verb(self, verb: str) -> None:
        response = await self._transport.call(
            "POST", path("sandboxes", self.id, verb), timeout=self._transport.read_bound(None)
        )
        self.info = sandbox_info(json_of(response))


class AsyncCommands:
    """The commands started in one sandbox, by this client or any other."""

    def __init__(self, transport: AsyncTransport, sandbox: str) -> None:
        self._transport = transport
        self._sandbox = sandbox

    async def list(self) -> builtins.list[CommandInfo]:
        records = await listed(self._transport, path("sandboxes", self._sandbox, "exec"), "execs")
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
        response = await self._transport.call("GET", path("sandboxes", self._sandbox, "exec", id))
        record = command_info(json_of(response))
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
            response = await self._transport.call(
                "GET", path("sandboxes", self.sandbox.id, "attach"), timeout=self._transport.read_bound(timeout)
            )
        except ShardConnectionError as e:
            if isinstance(e.__cause__, httpx.ReadTimeout):
                raise TimeoutError(f"the app of sandbox {self.sandbox.id} did not end within {timeout}s") from None
            raise
        return app_exit(json_of(response))

    async def logs(self) -> str:
        return await self.sandbox.logs()

    async def stop(self, *, force: bool = False) -> None:
        """End the app with TERM, or KILL with force, and cancel its restart policy."""
        await self._transport.call(
            "POST",
            path("sandboxes", self.sandbox.id, "app", "stop"),
            json={"force": True} if force else None,
            timeout=self._transport.read_bound(None),
        )


async def listed(
    transport: AsyncTransport, route: str, key: str, params: Mapping[str, str] | None = None
) -> builtins.list[Any]:
    """Every row of a paged list, one page after another."""
    query = dict(params or {})
    rows: builtins.list[Any] = []
    while True:
        page = json_of(await transport.call("GET", route, params=query))
        try:
            rows.extend(page[key])
            cursor = page["next"]
        except (KeyError, TypeError):
            raise ProtocolError(f"GET {route} answered a page with no {key} or next") from None
        if not cursor:
            return rows
        query["cursor"] = str(cursor)
