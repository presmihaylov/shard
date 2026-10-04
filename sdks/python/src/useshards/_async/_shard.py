"""The client: sandboxes made and found, and the policies, secrets and snapshots they draw on."""

from __future__ import annotations

import builtins
import warnings
from collections.abc import Mapping, Sequence
from types import TracebackType
from typing import Self

from .. import _types
from .._config import PLAIN_WARNING, StrPath, resolve
from .._generated import models
from .._generated.api.meta import get_capabilities, get_version
from .._generated.api.policies import get_policy, list_policies, put_policy, remove_policy
from .._generated.api.sandboxes import (
    attach_policy,
    create_sandbox,
    detach_policy,
    get_sandbox,
    grant_secret,
    list_sandboxes,
    ungrant_secret,
)
from .._generated.api.secrets import list_secrets, put_secret, remove_secret
from .._generated.api.snapshots import create_snapshot, get_snapshot, list_snapshots, remove_snapshot
from .._generated.types import UNSET
from .._types import Capabilities, Policy, PolicyRule, Restart, SandboxInfo, SecretInfo, Snapshot, Version
from .._wire import AsyncCall, create_body
from ._sandbox import AsyncApp, AsyncSandbox
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
        record = await self._transport.answer(
            models.Inspection, lambda: get_sandbox.asyncio_detailed(ref, client=self._transport.api)
        )
        return AsyncSandbox(self._transport, _types.sandbox_info(record))

    async def list(self, *, all: bool = False) -> builtins.list[AsyncSandbox]:
        """The running sandboxes, or with all every sandbox the daemon holds a record of."""
        records = await self._transport.listed(
            models.SandboxesResponse,
            lambda cursor: list_sandboxes.asyncio_detailed(
                client=self._transport.api, all_=all or UNSET, cursor=cursor
            ),
            lambda page: page.sandboxes,
        )
        return [AsyncSandbox(self._transport, _types.sandbox_info(record)) for record in records]

    async def version(self) -> Version:
        return _types.version(
            await self._transport.answer(
                models.VersionResponse, lambda: get_version.asyncio_detailed(client=self._transport.api)
            )
        )

    async def capabilities(self) -> Capabilities:
        """Each of the eight lifecycle verbs, and whether this server runs it."""
        return _types.capabilities(
            await self._transport.answer(
                models.Capabilities, lambda: get_capabilities.asyncio_detailed(client=self._transport.api)
            )
        )

    async def _create(self, body: models.CreateRequest) -> SandboxInfo:
        record = await self._transport.answer(
            models.Sandbox,
            lambda: create_sandbox.asyncio_detailed(client=self._transport.api, body=body, wait=True),
            self._transport.read_bound(None),
        )
        return _types.sandbox_info(record)


class AsyncPolicies:
    """Named egress policies, and which sandbox enforces which."""

    def __init__(self, transport: AsyncTransport) -> None:
        self._transport = transport

    async def set(self, name: str, rules: Sequence[PolicyRule]) -> Policy:
        """Make the policy, or replace every rule of it; a sandbox it is assigned to enforces the new rules."""
        body = models.PolicyRequest(
            rules=[models.RuleText(action=models.RuleTextAction(rule.action), rule=rule.rule) for rule in rules]
        )
        record = await self._transport.answer(
            models.PolicyView, lambda: put_policy.asyncio_detailed(name, client=self._transport.api, body=body)
        )
        return _types.policy(record)

    async def get(self, name: str) -> Policy:
        record = await self._transport.answer(
            models.PolicyView, lambda: get_policy.asyncio_detailed(name, client=self._transport.api)
        )
        return _types.policy(record)

    async def list(self) -> builtins.list[Policy]:
        """Every policy with its rules; holders and dns come only from get()."""
        records = await self._transport.listed(
            models.PoliciesResponse,
            lambda cursor: list_policies.asyncio_detailed(client=self._transport.api, cursor=cursor),
            lambda page: page.policies,
        )
        return [_types.policy(record) for record in records]

    async def remove(self, name: str) -> None:
        await self._transport.send(lambda: remove_policy.asyncio_detailed(name, client=self._transport.api))

    async def assign(self, sandbox: SandboxRef, name: str) -> SandboxInfo:
        body = models.PolicyAttachRequest(policy=name)
        return await _changed(
            self._transport,
            sandbox,
            lambda: attach_policy.asyncio_detailed(_id(sandbox), client=self._transport.api, body=body),
        )

    async def clear(self, sandbox: SandboxRef) -> SandboxInfo:
        return await _changed(
            self._transport, sandbox, lambda: detach_policy.asyncio_detailed(_id(sandbox), client=self._transport.api)
        )


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
        body = models.SecretRequest(
            value=value,
            destinations=UNSET if destinations is None else builtins.list(destinations),
            placeholder=UNSET if placeholder is None else placeholder,
        )
        record = await self._transport.answer(
            models.Secret, lambda: put_secret.asyncio_detailed(name, client=self._transport.api, body=body)
        )
        return _types.secret_info(record)

    async def list(self) -> builtins.list[SecretInfo]:
        records = await self._transport.listed(
            models.SecretsResponse,
            lambda cursor: list_secrets.asyncio_detailed(client=self._transport.api, cursor=cursor),
            lambda page: page.secrets,
        )
        return [_types.secret_info(record) for record in records]

    async def remove(self, name: str, *, force: bool = False) -> None:
        """Remove a secret no sandbox is granted; force revokes it from each first."""
        await self._transport.send(
            lambda: remove_secret.asyncio_detailed(name, client=self._transport.api, force=force or UNSET)
        )

    async def grant(self, sandbox: SandboxRef, name: str) -> SandboxInfo:
        return await _changed(
            self._transport,
            sandbox,
            lambda: grant_secret.asyncio_detailed(_id(sandbox), name, client=self._transport.api),
        )

    async def revoke(self, sandbox: SandboxRef, name: str) -> SandboxInfo:
        return await _changed(
            self._transport,
            sandbox,
            lambda: ungrant_secret.asyncio_detailed(_id(sandbox), name, client=self._transport.api),
        )


class AsyncSnapshots:
    """Snapshots of stopped sandboxes, which create(snapshot=) makes new sandboxes from."""

    def __init__(self, transport: AsyncTransport) -> None:
        self._transport = transport

    async def create(self, sandbox: SandboxRef, *, name: str | None = None) -> Snapshot:
        body = models.SnapshotRequest(sandbox=_id(sandbox), name=name or UNSET)
        record = await self._transport.answer(
            models.Snapshot,
            lambda: create_snapshot.asyncio_detailed(client=self._transport.api, body=body),
            self._transport.read_bound(None),
        )
        return _types.snapshot(record)

    async def list(self) -> builtins.list[Snapshot]:
        records = await self._transport.listed(
            models.SnapshotsResponse,
            lambda cursor: list_snapshots.asyncio_detailed(client=self._transport.api, cursor=cursor),
            lambda page: page.snapshots,
        )
        return [_types.snapshot(record) for record in records]

    async def inspect(self, ref: str) -> Snapshot:
        """A snapshot by id, id prefix or name."""
        record = await self._transport.answer(
            models.Snapshot, lambda: get_snapshot.asyncio_detailed(ref, client=self._transport.api)
        )
        return _types.snapshot(record)

    async def remove(self, ref: str) -> None:
        await self._transport.send(lambda: remove_snapshot.asyncio_detailed(ref, client=self._transport.api))


def _id(sandbox: SandboxRef) -> str:
    return sandbox.id if isinstance(sandbox, AsyncSandbox) else sandbox


async def _changed(transport: AsyncTransport, sandbox: SandboxRef, call: AsyncCall) -> SandboxInfo:
    """Send a change to a sandbox's record, and keep a handle's info current with the answer."""
    info = _types.sandbox_info(await transport.answer(models.Sandbox, call))
    if isinstance(sandbox, AsyncSandbox):
        sandbox.info = info
    return info
