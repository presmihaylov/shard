"""The plain records the SDK answers, with no connection behind them."""

from __future__ import annotations

import datetime
from collections.abc import Callable, Mapping
from dataclasses import dataclass
from typing import Any, Generic, Literal, TypeVar, get_args

import attrs

from ._generated import models
from ._generated.types import Unset
from .errors import ProtocolError

T = TypeVar("T")

OutputCallback = Callable[[bytes], None]
Signal = Literal["TERM", "KILL"]


@attrs.frozen
class TerminalSize:
    rows: int
    cols: int


@attrs.frozen
class CommandResult:
    """How a command ended. The output keeps only the newest bytes under the limit; lost_bytes is the daemon's loss."""

    id: str
    exit_code: int
    signal: int | None
    stdout_bytes: bytes = attrs.field(repr=False)
    stderr_bytes: bytes = attrs.field(repr=False)
    lost_bytes: int

    @property
    def stdout(self) -> str:
        return self.stdout_bytes.decode("utf-8", "replace")

    @property
    def stderr(self) -> str:
        return self.stderr_bytes.decode("utf-8", "replace")


@attrs.frozen
class CommandInfo:
    """One command's record as the daemon holds it, with no output."""

    id: str
    sandbox: str
    command: tuple[str, ...]
    state: Literal["running", "exited"]
    exit_code: int | None
    signal: int | None
    started_at: datetime.datetime
    exited_at: datetime.datetime | None
    lost_bytes: int


def command_info(record: models.Exec) -> CommandInfo:
    return CommandInfo(
        id=record.exec_,
        sandbox=record.sandbox,
        command=tuple(record.command),
        state=_one_of(_COMMAND_STATES, record.state, "a command state"),
        exit_code=None if record.exit_status is None else record.exit_status.code,
        signal=None if record.exit_status is None else record.exit_status.signal or None,
        started_at=record.started_at,
        exited_at=record.exited_at,
        lost_bytes=record.lost_bytes,
    )


FileType = Literal["file", "dir", "symlink", "other"]


@attrs.frozen
class FileInfo:
    """One sandbox path as the guest sees it; mode is the permission bits with setuid, setgid and sticky."""

    type: FileType
    size: int
    mode: int
    uid: int
    gid: int
    mtime: datetime.datetime


@attrs.frozen
class FileEntry:
    """One name in a sandbox directory with its own stat, never what a symlink points to."""

    name: str
    type: FileType
    size: int
    mode: int
    uid: int
    gid: int
    mtime: datetime.datetime


def file_info(record: Any) -> FileInfo:
    """The X-Shard-Stat header, which no model in the spec describes."""
    try:
        return FileInfo(
            type=_one_of(_FILE_TYPES, record["type"], "a file type"),
            size=int(record["size"]),
            mode=int(record["mode"]),
            uid=int(record["uid"]),
            gid=int(record["gid"]),
            mtime=datetime.datetime.fromisoformat(record["mtime"]),
        )
    except (KeyError, TypeError, ValueError) as e:
        raise ProtocolError(f"the daemon answered a file stat the SDK cannot read: {e!r}") from None


def file_entry(record: models.FileEntry) -> FileEntry:
    return FileEntry(
        name=record.name,
        type=_one_of(_FILE_TYPES, record.type_, "a file type"),
        size=record.size,
        mode=record.mode,
        uid=record.uid,
        gid=record.gid,
        mtime=record.mtime,
    )


@attrs.frozen
class Version:
    version: str
    api_version: str


@attrs.frozen
class Capabilities:
    """Whether the server runs each lifecycle verb; snapshot is creating a filesystem snapshot."""

    create: bool
    start: bool
    stop: bool
    remove: bool
    pause: bool
    resume: bool
    fork: bool
    snapshot: bool


@attrs.frozen
class ExitStatus:
    code: int
    signal: int | None


@attrs.frozen
class Resources:
    memory_mib: int
    vcpus: int
    disk_mib: int


RestartPolicy = Literal["no", "on-failure", "always"]


@attrs.frozen
class Restart:
    """When a run's app starts again. retries caps on-failure, 0 for no cap; backoff is seconds between starts."""

    policy: RestartPolicy
    retries: int = 0
    backoff: int = 0


@attrs.frozen
class RestartInfo:
    """The app's policy, and the starts again it made; ended is true once the policy starts it no more."""

    policy: RestartPolicy
    retries: int
    backoff: int
    count: int
    last_at: datetime.datetime | None
    gave_up: bool
    ended: bool


@attrs.frozen
class AppInfo:
    command: tuple[str, ...]
    exit_status: ExitStatus | None
    restart: RestartInfo | None


@attrs.frozen
class AppExit:
    """How a run's app ended once its restart policy was over."""

    exit_code: int
    signal: int | None
    restarts: int


@attrs.frozen
class SandboxInfo:
    """A sandbox's record. app is None for a sandbox made with no command, kernel on a container substrate."""

    id: str
    name: str | None
    image: str
    digest: str | None
    snapshot: str | None
    provider: str
    kernel: str | None
    state: str
    stopped_reason: str | None
    failed_reason: str | None
    resources: Resources
    app: AppInfo | None
    secrets: tuple[str, ...]
    policy: str | None
    started_at: datetime.datetime | None
    created_at: datetime.datetime


@attrs.frozen
class PolicyRule:
    """One rule as the CLI spells it, as in allow suffix:example.com tcp:443."""

    action: Literal["allow", "deny"]
    rule: str


@attrs.frozen
class Policy:
    """A named policy. holders are the sandboxes it is attached to, dns "open" or "closed"; a list leaves both None."""

    name: str
    rules: tuple[PolicyRule, ...]
    holders: tuple[str, ...] | None
    dns: str | None


@attrs.frozen
class SecretInfo:
    """A secret with no value: the SDK never reads one back."""

    name: str
    destinations: tuple[str, ...]
    placeholder: str
    updated_at: datetime.datetime


@dataclass(frozen=True)
class SandboxList(Generic[T]):
    sandboxes: list[T]
    warnings: list[str]


@dataclass(frozen=True)
class SecretList:
    secrets: list[SecretInfo]
    warnings: list[str]


def warning_lines(value: list[str] | Unset) -> list[str]:
    if isinstance(value, Unset):
        return []
    if not isinstance(value, list) or any(not isinstance(line, str) for line in value):
        raise ProtocolError("the daemon answered list warnings that are not strings")
    return value


@attrs.frozen
class Snapshot:
    id: str
    name: str | None
    source: str
    source_name: str | None
    image: str
    digest: str
    provider: str
    disk_mib: int
    memory_mib: int
    size: int
    created_at: datetime.datetime


@attrs.frozen
class EgressDecision:
    """One egress decision. rule is the id of the rule that decided it, rule_text that rule as the CLI spells it."""

    time: datetime.datetime
    source: str
    verdict: str
    host: str | None
    port: int | None
    address: str | None
    rule: str
    rule_text: str | None
    reason: str | None


_COMMAND_STATES: dict[str, Literal["running", "exited"]] = {"running": "running", "exited": "exited"}
_FILE_TYPES: dict[str, FileType] = {kind: kind for kind in get_args(FileType)}
_RESTART_POLICIES: dict[str, RestartPolicy] = {policy: policy for policy in get_args(RestartPolicy)}
_ACTIONS: dict[str, Literal["allow", "deny"]] = {"allow": "allow", "deny": "deny"}


def version(record: models.VersionResponse) -> Version:
    return Version(version=record.version, api_version=record.api_version)


def capabilities(record: models.Capabilities) -> Capabilities:
    return Capabilities(
        create=record.create,
        start=record.start,
        stop=record.stop,
        remove=record.remove,
        pause=record.pause,
        resume=record.resume,
        fork=record.fork,
        snapshot=record.snapshot,
    )


def sandbox_info(record: models.Sandbox | models.Inspection) -> SandboxInfo:
    app = None
    if record.command:
        app = AppInfo(
            command=tuple(record.command),
            exit_status=_exit_status(record.exit_status),
            restart=_restart(record.restart),
        )
    return SandboxInfo(
        id=record.id,
        name=record.name or None,
        image=record.image,
        digest=record.digest or None,
        snapshot=record.snapshot or None,
        provider=record.provider,
        kernel=record.kernel or None,
        state=record.state.value,
        stopped_reason=record.stopped_reason or None,
        failed_reason=record.failed_reason or None,
        resources=Resources(
            memory_mib=record.resources.memory_mib, vcpus=record.resources.vcpus, disk_mib=record.resources.disk_mib
        ),
        app=app,
        secrets=tuple(record.secrets or ()),
        policy=record.policy or None,
        started_at=_time_or_none(record.started_at),
        created_at=record.created_at,
    )


def app_exit(record: models.AppExit) -> AppExit:
    return AppExit(exit_code=record.code, signal=record.signal or None, restarts=record.restarts)


def policy(record: models.Policy | models.PolicyView) -> Policy:
    """A get answers holders and dns; a list row answers neither, so both stay None."""
    return Policy(
        name=record.name,
        rules=tuple(_policy_rule(rule) for rule in record.rules),
        holders=tuple(record.holders or ()) if isinstance(record, models.PolicyView) else None,
        dns=record.dns.value if isinstance(record, models.PolicyView) else None,
    )


def secret_info(record: models.Secret) -> SecretInfo:
    return SecretInfo(
        name=record.name,
        destinations=tuple(record.destinations),
        placeholder=record.placeholder,
        updated_at=record.updated_at,
    )


def snapshot(record: models.Snapshot) -> Snapshot:
    return Snapshot(
        id=record.id,
        name=record.name or None,
        source=record.source,
        source_name=record.source_name or None,
        image=record.image,
        digest=record.digest,
        provider=record.provider,
        disk_mib=record.disk_mib,
        memory_mib=record.memory_mib,
        size=record.size,
        created_at=record.created_at,
    )


def egress_decision(record: models.EgressDecision) -> EgressDecision:
    return EgressDecision(
        time=record.time,
        source=record.source.value,
        verdict=record.verdict.value,
        host=record.host or None,
        port=record.port or None,
        address=record.address or None,
        rule=record.rule,
        rule_text=record.rule_text or None,
        reason=record.reason or None,
    )


def _exit_status(record: models.ExitStatus | Unset) -> ExitStatus | None:
    if isinstance(record, Unset):
        return None
    return ExitStatus(code=record.code, signal=record.signal or None)


def _restart(record: models.Restart | Unset) -> RestartInfo | None:
    if isinstance(record, Unset):
        return None
    return RestartInfo(
        policy=_one_of(_RESTART_POLICIES, record.policy, "a restart policy"),
        retries=record.retries or 0,
        backoff=record.backoff or 0,
        count=record.count,
        last_at=_time_or_none(record.last_at),
        gave_up=record.gave_up,
        ended=record.ended,
    )


def _policy_rule(record: models.Rule) -> PolicyRule:
    # Spelled as the daemon's FormatRule spells it, less the action, so a set and its get compare equal.
    text = record.destination.value
    if record.destination.kind == "domain-suffix":
        text = f"suffix:{text}"
    if record.protocol:
        text += f" {record.protocol}"
        if record.ports:
            text += ":" + ",".join(str(port) for port in record.ports)
    return PolicyRule(action=_one_of(_ACTIONS, record.action, "a rule action"), rule=text)


def _time_or_none(value: datetime.datetime | Unset) -> datetime.datetime | None:
    return None if isinstance(value, Unset) else value


def _one_of(table: Mapping[str, T], value: str, what: str) -> T:
    try:
        return table[value]
    except KeyError:
        raise ProtocolError(f"the daemon answered {what} the SDK does not know: {value!r}") from None
