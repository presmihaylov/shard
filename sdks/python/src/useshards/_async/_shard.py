"""The client: sandboxes made and found, and the policies, secrets and snapshots they draw on."""

from __future__ import annotations

import builtins
import warnings
from collections.abc import Mapping, Sequence
from types import TracebackType
from typing import Self

from .. import _types
from .._config import PLAIN_WARNING, StrPath, resolve
from .._types import Capabilities, Policy, PolicyRule, Restart, SandboxInfo, SecretInfo, Snapshot, Version
from .._wire import create_body, json_of, path
from ._sandbox import AsyncApp, AsyncSandbox, listed
from ._transport import DEFAULT_TIMEOUT, AsyncTransport

SandboxRef = AsyncSandbox | str


class AsyncShard:
    """A client of one shard daemon. remote and api_key default to SHARD_REMOTE and SHARD_API_KEY."""

    def __init__(
        self,
        remote: str | None = None,
        api_key: str | None = None,
        *,
        ca_file: StrPath | None = None,
        timeout: float | None = DEFAULT_TIMEOUT,
    ) -> None:
        settings = resolve(remote, api_key, ca_file)
        if settings.plain:
            warnings.warn(PLAIN_WARNING, stacklevel=2)
        self._transport = AsyncTransport(settings, timeout)
        self.policies = AsyncPolicies(self._transport)
        self.secrets = AsyncSecrets(self._transport)
        self.snapshots = AsyncSnapshots(self._transport)

    async def __aenter__(self) -> Self:
        return self

    async def __aexit__(
        self, kind: type[BaseException] | None, error: BaseException | None, trace: TracebackType | None
    ) -> None:
        await self.aclose()

    async def aclose(self) -> None:
        await self._transport.aclose()

    async def create(
        self,
        image: str | None = None,
        *,
        snapshot: str | None = None,
        name: str | None = None,
        env: Mapping[str, str] | None = None,
        workdir: str | None = None,
        user: str | None = None,
        secrets: Sequence[str] | None = None,
        policy: str | None = None,
        memory_mib: int | None = None,
        vcpus: int | None = None,
        disk_mib: int | None = None,
    ) -> AsyncSandbox:
        """A running sandbox with no app, from an image or a snapshot; exec() runs in it."""
        body = create_body(
            image,
            None,
            snapshot=snapshot,
            name=name,
            env=env,
            workdir=workdir,
            user=user,
            secrets=secrets,
            policy=policy,
            memory_mib=memory_mib,
            vcpus=vcpus,
            disk_mib=disk_mib,
            restart=None,
        )
        return AsyncSandbox(self._transport, await self._create(body))

    async def run(
        self,
        image: str,
        command: str | Sequence[str],
        *,
        name: str | None = None,
        env: Mapping[str, str] | None = None,
        workdir: str | None = None,
        user: str | None = None,
        secrets: Sequence[str] | None = None,
        policy: str | None = None,
        memory_mib: int | None = None,
        vcpus: int | None = None,
        disk_mib: int | None = None,
        restart: Restart | None = None,
    ) -> AsyncApp:
        """A sandbox whose app is command. The sandbox outlives the app; remove() it when done."""
        body = create_body(
            image,
            command,
            snapshot=None,
            name=name,
            env=env,
            workdir=workdir,
            user=user,
            secrets=secrets,
            policy=policy,
            memory_mib=memory_mib,
            vcpus=vcpus,
            disk_mib=disk_mib,
            restart=restart,
        )
        return AsyncApp(self._transport, AsyncSandbox(self._transport, await self._create(body)))

    async def get(self, ref: str) -> AsyncSandbox:
        """A sandbox by id, id prefix or name."""
        response = await self._transport.call("GET", path("sandboxes", ref))
        return AsyncSandbox(self._transport, _types.sandbox_info(json_of(response)))

    async def list(self, *, all: bool = False) -> builtins.list[AsyncSandbox]:
        """The running sandboxes, or with all every sandbox the daemon holds a record of."""
        records = await listed(self._transport, path("sandboxes"), "sandboxes", {"all": "true"} if all else None)
        return [AsyncSandbox(self._transport, _types.sandbox_info(record)) for record in records]

    async def version(self) -> Version:
        return _types.version(json_of(await self._transport.call("GET", path("version"))))

    async def capabilities(self) -> Capabilities:
        return _types.capabilities(json_of(await self._transport.call("GET", path("capabilities"))))

    async def _create(self, body: dict[str, object]) -> SandboxInfo:
        response = await self._transport.call(
            "POST", path("sandboxes"), json=body, params={"wait": "true"}, timeout=self._transport.read_bound(None)
        )
        return _types.sandbox_info(json_of(response))


class AsyncPolicies:
    """Named egress policies, and which sandbox enforces which."""

    def __init__(self, transport: AsyncTransport) -> None:
        self._transport = transport

    async def set(self, name: str, rules: Sequence[PolicyRule]) -> Policy:
        """Make the policy, or replace every rule of it; a sandbox it is assigned to enforces the new rules."""
        body = {"rules": [{"action": rule.action, "rule": rule.rule} for rule in rules]}
        return _types.policy(json_of(await self._transport.call("PUT", path("policies", name), json=body)))

    async def get(self, name: str) -> Policy:
        return _types.policy(json_of(await self._transport.call("GET", path("policies", name))))

    async def list(self) -> builtins.list[Policy]:
        """Every policy with its rules; holders and dns come only from get()."""
        return [_types.policy(record) for record in await listed(self._transport, path("policies"), "policies")]

    async def remove(self, name: str) -> None:
        await self._transport.call("DELETE", path("policies", name))

    async def assign(self, sandbox: SandboxRef, name: str) -> SandboxInfo:
        return await _changed(
            self._transport, sandbox, "PUT", path("sandboxes", _id(sandbox), "policy"), {"policy": name}
        )

    async def clear(self, sandbox: SandboxRef) -> SandboxInfo:
        return await _changed(self._transport, sandbox, "DELETE", path("sandboxes", _id(sandbox), "policy"))


class AsyncSecrets:
    """Secrets by name. The SDK sends a value and never reads one back."""

    def __init__(self, transport: AsyncTransport) -> None:
        self._transport = transport

    async def set(
        self,
        name: str,
        *,
        value: str,
        destinations: Sequence[str] | None = None,
        placeholder: str | None = None,
    ) -> SecretInfo:
        """Make the secret or replace it. A sandbox sees only the placeholder; the proxy swaps in the value."""
        body: dict[str, object] = {"value": value}
        if destinations is not None:
            body["destinations"] = builtins.list(destinations)
        if placeholder is not None:
            body["placeholder"] = placeholder
        return _types.secret_info(json_of(await self._transport.call("PUT", path("secrets", name), json=body)))

    async def list(self) -> builtins.list[SecretInfo]:
        return [_types.secret_info(record) for record in await listed(self._transport, path("secrets"), "secrets")]

    async def remove(self, name: str, *, force: bool = False) -> None:
        """Remove a secret no sandbox is granted; force revokes it from each first."""
        await self._transport.call("DELETE", path("secrets", name), params={"force": "true"} if force else None)

    async def grant(self, sandbox: SandboxRef, name: str) -> SandboxInfo:
        return await _changed(self._transport, sandbox, "POST", path("sandboxes", _id(sandbox), "secrets", name))

    async def revoke(self, sandbox: SandboxRef, name: str) -> SandboxInfo:
        return await _changed(self._transport, sandbox, "DELETE", path("sandboxes", _id(sandbox), "secrets", name))


class AsyncSnapshots:
    """Snapshots of stopped sandboxes, which create(snapshot=) makes new sandboxes from."""

    def __init__(self, transport: AsyncTransport) -> None:
        self._transport = transport

    async def create(self, sandbox: SandboxRef, *, name: str | None = None) -> Snapshot:
        body = {"sandbox": _id(sandbox)}
        if name:
            body["name"] = name
        response = await self._transport.call(
            "POST", path("snapshots"), json=body, timeout=self._transport.read_bound(None)
        )
        return _types.snapshot(json_of(response))

    async def list(self) -> builtins.list[Snapshot]:
        return [_types.snapshot(record) for record in await listed(self._transport, path("snapshots"), "snapshots")]

    async def inspect(self, ref: str) -> Snapshot:
        """A snapshot by id, id prefix or name."""
        return _types.snapshot(json_of(await self._transport.call("GET", path("snapshots", ref))))

    async def remove(self, ref: str) -> None:
        await self._transport.call("DELETE", path("snapshots", ref))


def _id(sandbox: SandboxRef) -> str:
    return sandbox.id if isinstance(sandbox, AsyncSandbox) else sandbox


async def _changed(
    transport: AsyncTransport, sandbox: SandboxRef, method: str, route: str, body: object = None
) -> SandboxInfo:
    """Send a change to a sandbox's record, and keep a handle's info current with the answer."""
    info = _types.sandbox_info(json_of(await transport.call(method, route, json=body)))
    if isinstance(sandbox, AsyncSandbox):
        sandbox.info = info
    return info
