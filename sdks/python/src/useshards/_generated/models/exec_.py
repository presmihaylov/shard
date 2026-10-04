from __future__ import annotations

import datetime
from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.exec_state import ExecState
from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.exit_status import ExitStatus


T = TypeVar("T", bound="Exec")


@_attrs_define
class Exec:
    command: list[str]
    exec_: str
    exit_status: ExitStatus | None
    exited_at: datetime.datetime | None
    lost_bytes: int
    sandbox: str
    started_at: datetime.datetime
    state: ExecState
    truncated: bool

    def to_dict(self) -> dict[str, Any]:
        from ..models.exit_status import ExitStatus  # noqa: PLC0415

        command = self.command

        exec_ = self.exec_

        exit_status: dict[str, Any] | None
        if isinstance(self.exit_status, ExitStatus):
            exit_status = self.exit_status.to_dict()
        else:
            exit_status = self.exit_status

        exited_at: None | str
        if isinstance(self.exited_at, datetime.datetime):
            exited_at = self.exited_at.isoformat()
        else:
            exited_at = self.exited_at

        lost_bytes = self.lost_bytes

        sandbox = self.sandbox

        started_at = self.started_at.isoformat()

        state = self.state.value

        truncated = self.truncated

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "command": command,
                "exec": exec_,
                "exit_status": exit_status,
                "exited_at": exited_at,
                "lost_bytes": lost_bytes,
                "sandbox": sandbox,
                "started_at": started_at,
                "state": state,
                "truncated": truncated,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.exit_status import ExitStatus  # noqa: PLC0415

        d = dict(src_dict)
        command = cast(list[str], d.pop("command"))

        exec_ = d.pop("exec")

        def _parse_exit_status(data: object) -> ExitStatus | None:
            if data is None:
                return data
            try:
                if not isinstance(data, dict):
                    raise TypeError()
                exit_status_type_0 = ExitStatus.from_dict(data)

                return exit_status_type_0
            except (TypeError, ValueError, AttributeError, KeyError):
                pass
            return cast(ExitStatus | None, data)

        exit_status = _parse_exit_status(d.pop("exit_status"))

        def _parse_exited_at(data: object) -> datetime.datetime | None:
            if data is None:
                return data
            try:
                if not isinstance(data, str):
                    raise TypeError()
                exited_at_type_0 = datetime.datetime.fromisoformat(data)

                return exited_at_type_0
            except (TypeError, ValueError, AttributeError, KeyError):
                pass
            return cast(datetime.datetime | None, data)

        exited_at = _parse_exited_at(d.pop("exited_at"))

        lost_bytes = d.pop("lost_bytes")

        sandbox = d.pop("sandbox")

        started_at = datetime.datetime.fromisoformat(d.pop("started_at"))

        state = ExecState(d.pop("state"))

        truncated = d.pop("truncated")

        exec_ = cls(
            command=command,
            exec_=exec_,
            exit_status=exit_status,
            exited_at=exited_at,
            lost_bytes=lost_bytes,
            sandbox=sandbox,
            started_at=started_at,
            state=state,
            truncated=truncated,
        )

        return exec_
