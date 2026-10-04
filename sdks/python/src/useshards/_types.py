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
