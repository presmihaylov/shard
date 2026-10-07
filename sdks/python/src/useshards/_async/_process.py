"""The named processes a run starts in a sandbox. Each ends on its own or by kill(), and the sandbox outlives it."""

from __future__ import annotations

import builtins
from collections.abc import Mapping, Sequence

import httpx

from .._generated import models
from .._generated.api.processes import (
    attach_process,
    get_process,
    get_process_logs,
    kill_process,
    list_processes,
    run_process,
)
from .._generated.types import UNSET
from .._types import ProcessInfo, Restart, process_info
from .._wire import path, run_body
from ..errors import ShardConnectionError
from ._follow import AsyncFollow, log_chunk
from ._transport import AsyncTransport


class AsyncProcesses:
    """The named processes of one sandbox, run by this client or any other."""

    def __init__(self, transport: AsyncTransport, sandbox: str) -> None:
        self._transport = transport
        self._sandbox = sandbox

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
        body = run_body(command, name=name, env=env, workdir=workdir, user=user, restart=restart)
        record = await self._transport.answer(
            models.Process,
            lambda: run_process.asyncio_detailed(self._sandbox, client=self._transport.api, body=body),
            self._transport.read_bound(None),
        )
        return AsyncProcess(self._transport, self._sandbox, process_info(record))

    async def list(self) -> builtins.list[ProcessInfo]:
        """list the processes of a sandbox, in the order they were first run"""
        page = await self._transport.answer(
            models.ProcessesResponse, lambda: list_processes.asyncio_detailed(self._sandbox, client=self._transport.api)
        )
        return [process_info(record) for record in page.processes]

    async def get(self, name: str) -> AsyncProcess:
        """Return a handle to the process of that name, as the daemon last read it."""
        record = await self._transport.answer(
            models.Process, lambda: get_process.asyncio_detailed(self._sandbox, name, client=self._transport.api)
        )
        return AsyncProcess(self._transport, self._sandbox, process_info(record))


class AsyncProcess:
    """One named process. info is the process as the last call on this handle returned it."""

    def __init__(self, transport: AsyncTransport, sandbox: str, info: ProcessInfo) -> None:
        self._transport = transport
        self.sandbox = sandbox
        self.info = info

    def __repr__(self) -> str:
        return f"{type(self).__name__}(sandbox={self.sandbox!r}, name={self.name!r}, state={self.info.status.state!r})"

    @property
    def name(self) -> str:
        return self.info.name

    async def inspect(self) -> ProcessInfo:
        record = await self._transport.answer(
            models.Process, lambda: get_process.asyncio_detailed(self.sandbox, self.name, client=self._transport.api)
        )
        self.info = process_info(record)
        return self.info

    async def wait(self, timeout: float | None = None) -> ProcessInfo:
        """Block until the process ends for good; a timeout ends the wait, never the process."""
        try:
            record = await self._transport.answer(
                models.Process,
                lambda: attach_process.asyncio_detailed(self.sandbox, self.name, client=self._transport.api),
                self._transport.read_bound(timeout),
            )
        except ShardConnectionError as e:
            if isinstance(e.__cause__, httpx.ReadTimeout):
                raise TimeoutError(
                    f"process {self.name} of sandbox {self.sandbox} did not end within {timeout}s"
                ) from None
            raise
        self.info = process_info(record)
        return self.info

    async def logs(self) -> str:
        """Return every run's output the host keeps, stdout and stderr as the process wrote them."""
        return await self._transport.answer(
            str, lambda: get_process_logs.asyncio_detailed(self.sandbox, self.name, client=self._transport.api)
        )

    def follow_logs(self) -> AsyncFollow[bytes]:
        """Yield the log from its start, then as it grows, until the process ends or its sandbox stops."""
        return AsyncFollow(
            self._transport,
            path("sandboxes", self.sandbox, "processes", self.name, "logs"),
            f"the logs of process {self.name} of sandbox {self.sandbox}",
            log_chunk,
        )

    async def kill(self, *, force: bool = False) -> ProcessInfo:
        """stop a process and cancel its restarts"""
        body = models.ProcessKillRequest(force=True) if force else UNSET
        record = await self._transport.answer(
            models.Process,
            lambda: kill_process.asyncio_detailed(self.sandbox, self.name, client=self._transport.api, body=body),
            self._transport.read_bound(None),
        )
        self.info = process_info(record)
        return self.info
