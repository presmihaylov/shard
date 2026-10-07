from __future__ import annotations

import datetime
from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.process_status_state import ProcessStatusState
from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.exit_status import ExitStatus


T = TypeVar("T", bound="ProcessStatus")


@_attrs_define
class ProcessStatus:
    restarts: int
    state: ProcessStatusState
    exit_: ExitStatus | Unset = UNSET
    started_at: datetime.datetime | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        from ..models.exit_status import ExitStatus  # noqa: PLC0415

        restarts = self.restarts

        state = self.state.value

        exit_: dict[str, Any] | Unset = UNSET
        if not isinstance(self.exit_, Unset):
            exit_ = self.exit_.to_dict()

        started_at: str | Unset = UNSET
        if not isinstance(self.started_at, Unset):
            started_at = self.started_at.isoformat()

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "restarts": restarts,
                "state": state,
            }
        )
        if exit_ is not UNSET:
            field_dict["exit"] = exit_
        if started_at is not UNSET:
            field_dict["started_at"] = started_at

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.exit_status import ExitStatus  # noqa: PLC0415

        d = dict(src_dict)
        restarts = d.pop("restarts")

        state = ProcessStatusState(d.pop("state"))

        _exit_ = d.pop("exit", UNSET)
        exit_: ExitStatus | Unset
        if isinstance(_exit_, Unset):
            exit_ = UNSET
        else:
            exit_ = ExitStatus.from_dict(_exit_)

        _started_at = d.pop("started_at", UNSET)
        started_at: datetime.datetime | Unset
        if isinstance(_started_at, Unset):
            started_at = UNSET
        else:
            started_at = datetime.datetime.fromisoformat(_started_at)

        process_status = cls(
            restarts=restarts,
            state=state,
            exit_=exit_,
            started_at=started_at,
        )

        return process_status
