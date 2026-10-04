"""The plain records the SDK answers, with no connection behind them."""

from __future__ import annotations

import datetime
from collections.abc import Callable
from typing import Any, Literal

import attrs

from .errors import ProtocolError

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


def command_info(record: Any) -> CommandInfo:
    try:
        exit_status = record["exit_status"]
        exited_at = record["exited_at"]
        state = record["state"]
        if state not in ("running", "exited"):
            raise ValueError(state)
        return CommandInfo(
            id=record["exec"],
            sandbox=record["sandbox"],
            command=tuple(record["command"]),
            state=state,
            exit_code=None if exit_status is None else int(exit_status["code"]),
            signal=None if exit_status is None else int(exit_status["signal"]) or None,
            started_at=datetime.datetime.fromisoformat(record["started_at"]),
            exited_at=None if exited_at is None else datetime.datetime.fromisoformat(exited_at),
            lost_bytes=int(record["lost_bytes"]),
        )
    except (KeyError, TypeError, ValueError) as e:
        raise ProtocolError(f"the daemon answered a command record the SDK cannot read: {e!r}") from None


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
    return FileInfo(**_stat(record))


def file_entry(record: Any) -> FileEntry:
    try:
        name = str(record["name"])
    except (KeyError, TypeError) as e:
        raise ProtocolError(f"the daemon answered a directory entry the SDK cannot read: {e!r}") from None
    return FileEntry(name=name, **_stat(record))


def _stat(record: Any) -> dict[str, Any]:
    try:
        kind = record["type"]
        if kind not in ("file", "dir", "symlink", "other"):
            raise ValueError(kind)
        return {
            "type": kind,
            "size": int(record["size"]),
            "mode": int(record["mode"]),
            "uid": int(record["uid"]),
            "gid": int(record["gid"]),
            "mtime": datetime.datetime.fromisoformat(record["mtime"]),
        }
    except (KeyError, TypeError, ValueError) as e:
        raise ProtocolError(f"the daemon answered a file stat the SDK cannot read: {e!r}") from None


@attrs.frozen
class Version:
    version: str
    api_version: str


@attrs.frozen
class Capabilities:
    """The daemon's provider, and every optional verb it refuses."""

    provider: str
    unsupported: tuple[str, ...]


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
    """A sandbox's record. app is None for a sandbox made with no command."""

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
    """A named policy. holders are the sandboxes it is assigned to, dns "open" or "closed"; a list leaves both None."""

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
class NetworkLogRecord:
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


def version(record: Any) -> Version:
    try:
        return Version(version=str(record["version"]), api_version=str(record["api_version"]))
    except (KeyError, TypeError) as e:
        raise _unreadable("a version", e) from None


def capabilities(record: Any) -> Capabilities:
    try:
        return Capabilities(
            provider=str(record["provider"]), unsupported=tuple(str(verb) for verb in record["unsupported"])
        )
    except (KeyError, TypeError) as e:
        raise _unreadable("the capabilities", e) from None


def sandbox_info(record: Any) -> SandboxInfo:
    try:
        resources = record["resources"]
        command = record.get("command")
        app = None
        if command:
            app = AppInfo(
                command=tuple(str(arg) for arg in command),
                exit_status=_exit_status(record.get("exit_status")),
                restart=_restart(record.get("restart")),
            )
        return SandboxInfo(
            id=str(record["id"]),
            name=record.get("name") or None,
            image=str(record["image"]),
            digest=record.get("digest") or None,
            snapshot=record.get("snapshot") or None,
            provider=str(record["provider"]),
            kernel=record.get("kernel") or None,
            state=str(record["state"]),
            stopped_reason=record.get("stopped_reason") or None,
            failed_reason=record.get("failed_reason") or None,
            resources=Resources(
                memory_mib=int(resources["memory_mib"]),
                vcpus=int(resources["vcpus"]),
                disk_mib=int(resources["disk_mib"]),
            ),
            app=app,
            secrets=tuple(str(name) for name in record.get("secrets") or ()),
            policy=record.get("policy") or None,
            started_at=_time_or_none(record.get("started_at")),
            created_at=datetime.datetime.fromisoformat(record["created_at"]),
        )
    except (KeyError, TypeError, ValueError) as e:
        raise _unreadable("a sandbox record", e) from None


def app_exit(record: Any) -> AppExit:
    try:
        return AppExit(
            exit_code=int(record["code"]), signal=int(record["signal"]) or None, restarts=int(record["restarts"])
        )
    except (KeyError, TypeError, ValueError) as e:
        raise _unreadable("an app exit", e) from None


def policy(record: Any) -> Policy:
    try:
        return Policy(
            name=str(record["name"]),
            rules=tuple(_policy_rule(rule) for rule in record["rules"]),
            holders=tuple(str(name) for name in record.get("holders") or ()) if "dns" in record else None,
            dns=str(record["dns"]) if "dns" in record else None,
        )
    except (KeyError, TypeError, ValueError) as e:
        raise _unreadable("a policy", e) from None


def secret_info(record: Any) -> SecretInfo:
    try:
        return SecretInfo(
            name=str(record["name"]),
            destinations=tuple(str(each) for each in record["destinations"]),
            placeholder=str(record["placeholder"]),
            updated_at=datetime.datetime.fromisoformat(record["updated_at"]),
        )
    except (KeyError, TypeError, ValueError) as e:
        raise _unreadable("a secret", e) from None


def snapshot(record: Any) -> Snapshot:
    try:
        return Snapshot(
            id=str(record["id"]),
            name=record.get("name") or None,
            source=str(record["source"]),
            source_name=record.get("source_name") or None,
            image=str(record["image"]),
            digest=str(record["digest"]),
            provider=str(record["provider"]),
            disk_mib=int(record["disk_mib"]),
            memory_mib=int(record["memory_mib"]),
            size=int(record["size"]),
            created_at=datetime.datetime.fromisoformat(record["created_at"]),
        )
    except (KeyError, TypeError, ValueError) as e:
        raise _unreadable("a snapshot", e) from None


def network_log_record(record: Any) -> NetworkLogRecord:
    try:
        return NetworkLogRecord(
            time=datetime.datetime.fromisoformat(record["time"]),
            source=str(record["source"]),
            verdict=str(record["verdict"]),
            host=record.get("host") or None,
            port=int(record.get("port") or 0) or None,
            address=record.get("address") or None,
            rule=str(record["rule"]),
            rule_text=record.get("rule_text") or None,
            reason=record.get("reason") or None,
        )
    except (KeyError, TypeError, ValueError) as e:
        raise _unreadable("a network log record", e) from None


def _exit_status(record: Any) -> ExitStatus | None:
    if record is None:
        return None
    return ExitStatus(code=int(record["code"]), signal=int(record["signal"]) or None)


def _restart(record: Any) -> RestartInfo | None:
    if record is None:
        return None
    policy = record["policy"]
    if policy not in ("no", "on-failure", "always"):
        raise ValueError(policy)
    return RestartInfo(
        policy=policy,
        retries=int(record.get("retries", 0)),
        backoff=int(record["backoff"]),
        count=int(record["count"]),
        last_at=_time_or_none(record.get("last_at")),
        gave_up=bool(record["gave_up"]),
        ended=bool(record["ended"]),
    )


def _policy_rule(record: Any) -> PolicyRule:
    action = record["action"]
    if action not in ("allow", "deny"):
        raise ValueError(action)
    destination = record["destination"]
    # Spelled as the daemon's FormatRule spells it, less the action, so a set and its get compare equal.
    text = str(destination["value"])
    if destination["kind"] == "domain-suffix":
        text = f"suffix:{text}"
    protocol = record.get("protocol")
    if protocol:
        text += f" {protocol}"
        ports = record.get("ports")
        if ports:
            text += ":" + ",".join(str(int(port)) for port in ports)
    return PolicyRule(action=action, rule=text)


def _time_or_none(value: Any) -> datetime.datetime | None:
    return None if value is None else datetime.datetime.fromisoformat(value)


def _unreadable(what: str, e: Exception) -> ProtocolError:
    return ProtocolError(f"the daemon answered {what} the SDK cannot read: {e!r}")
