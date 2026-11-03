"""One sandbox and what runs in it: its commands, the processes a run started, its files and its ports."""

from __future__ import annotations

import builtins
from collections.abc import Mapping, Sequence
from typing import Literal, overload

from .._capture import DEFAULT_OUTPUT_LIMIT
from .._generated import models
from .._generated.api.exec_ import get_exec, list_execs
from .._generated.api.ports import list_sandbox_ports, put_port, remove_port
from .._generated.api.sandboxes import (
    fork_sandbox,
    get_sandbox,
    get_sandbox_egress_log,
    pause_sandbox,
    remove_sandbox,
    resume_sandbox,
    start_sandbox,
    stop_sandbox,
)
from .._generated.types import UNSET
from .._types import (
    CommandInfo,
    CommandResult,
    EgressDecision,
    OutputCallback,
    Port,
    Restart,
    SandboxInfo,
    TerminalSize,
    command_info,
    egress_decision,
    port,
    sandbox_info,
)
from .._wire import AsyncCall, path
from ._command import AsyncCommand, run_command, start_command
from ._files import AsyncFiles
from ._follow import AsyncFollow, egress_log_entry
from ._process import AsyncProcess, AsyncProcesses
from ._transport import AsyncTransport


class AsyncSandbox:
    """One sandbox. info is the sandbox as the last call on this handle returned it; inspect() reads it again."""

    def __init__(self, transport: AsyncTransport, info: SandboxInfo) -> None:
        self._transport = transport
        self.info = info
        self.commands = AsyncCommands(transport, info.id)
        self.processes = AsyncProcesses(transport, info.id)
        self.files = AsyncFiles(transport, info.id)
        self.ports = AsyncPorts(transport, info.id)

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
        """execute a command in a running sandbox"""
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

    async def run(
        self,
        command: str | Sequence[str],
        *,
        name: str | None = None,
        env: Mapping[str, str] | None = None,
        workdir: str | None = None,
        user: str | None = None,
        restart: Restart | None = None,
    ) -> AsyncProcess:
        """start a supervised process in a running sandbox"""
        return await self.processes.run(command, name=name, env=env, workdir=workdir, user=user, restart=restart)

    async def stop(self) -> None:
        """stop a sandbox and preserve its files"""
        await self._verb(lambda: stop_sandbox.asyncio_detailed(self.id, client=self._transport.api))

    async def start(self) -> None:
        """start a stopped sandbox with its saved files"""
        await self._verb(lambda: start_sandbox.asyncio_detailed(self.id, client=self._transport.api))

    async def pause(self) -> None:
        """save a sandbox's state and suspend it"""
        await self._verb(lambda: pause_sandbox.asyncio_detailed(self.id, client=self._transport.api))

    async def resume(self) -> None:
        """resume a paused sandbox from its saved state"""
        await self._verb(lambda: resume_sandbox.asyncio_detailed(self.id, client=self._transport.api))

    async def fork(self, *, name: str | None = None) -> AsyncSandbox:
        """create a sandbox from a running sandbox's memory and files"""
        body = models.CopyRequest(name=name or UNSET)
        record = await self._transport.answer(
            models.Sandbox,
            lambda: fork_sandbox.asyncio_detailed(self.id, client=self._transport.api, body=body),
            self._transport.read_bound(None),
        )
        return AsyncSandbox(self._transport, sandbox_info(record))

    async def remove(self, *, force: bool = False) -> None:
        """remove a sandbox and its files"""
        await self._transport.send(
            lambda: remove_sandbox.asyncio_detailed(self.id, client=self._transport.api, force=force or UNSET),
            self._transport.read_bound(None),
        )

    async def egress_log(self) -> builtins.list[EgressDecision]:
        """Return the egress decisions the daemon still holds, oldest first."""
        records = await self._transport.answer(
            builtins.list, lambda: get_sandbox_egress_log.asyncio_detailed(self.id, client=self._transport.api)
        )
        return [egress_decision(record) for record in records]

    def follow_egress_log(self) -> AsyncFollow[EgressDecision]:
        """Yield each egress decision as the daemon makes it, and end when the sandbox stops."""
        return AsyncFollow(
            self._transport,
            path("sandboxes", self.id, "egress-log"),
            f"the egress log of sandbox {self.id}",
            egress_log_entry,
        )

    async def _verb(self, call: AsyncCall) -> None:
        self.info = sandbox_info(await self._transport.answer(models.Sandbox, call, self._transport.read_bound(None)))


class AsyncCommands:
    """The commands started in one sandbox, by this client or any other."""

    def __init__(self, transport: AsyncTransport, sandbox: str) -> None:
        self._transport = transport
        self._sandbox = sandbox

    async def list(self) -> builtins.list[CommandInfo]:
        """Return every command the daemon still holds for the sandbox, running or ended."""
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
        """Return a handle to a command any client started; its wait() attaches, so the callbacks see the replay."""
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


class AsyncPorts:
    """The host ports one sandbox forwards; a running sandbox listens at once, any other from its next start."""

    def __init__(self, transport: AsyncTransport, sandbox: str) -> None:
        self._transport = transport
        self._sandbox = sandbox

    async def add(self, host_port: int, guest_port: int, *, public: bool = False) -> Port:
        """Upsert host_port to guest_port; public binds every host interface instead of 127.0.0.1."""
        body = models.PortRequest(guest_port=guest_port, public=public or UNSET)
        record = await self._transport.answer(
            models.Port,
            lambda: put_port.asyncio_detailed(self._sandbox, host_port, client=self._transport.api, body=body),
        )
        return port(record)

    async def list(self) -> builtins.list[Port]:
        """Return the sandbox's forwards in host port order, each as the host serves it now."""
        records = await self._transport.listed(
            models.PortsResponse,
            lambda cursor: list_sandbox_ports.asyncio_detailed(
                self._sandbox, client=self._transport.api, cursor=cursor
            ),
            lambda page: page.ports,
        )
        return [port(record) for record in records]

    async def remove(self, host_port: int) -> None:
        """End the forward on host_port, and the connections it carries."""
        await self._transport.send(
            lambda: remove_port.asyncio_detailed(self._sandbox, host_port, client=self._transport.api)
        )
