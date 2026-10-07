from __future__ import annotations

import datetime
from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="OOM")


@_attrs_define
class OOM:
    killed_at: datetime.datetime
    kills: int
    restart_at: datetime.datetime | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        killed_at = self.killed_at.isoformat()

        kills = self.kills

        restart_at: str | Unset = UNSET
        if not isinstance(self.restart_at, Unset):
            restart_at = self.restart_at.isoformat()

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "killed_at": killed_at,
                "kills": kills,
            }
        )
        if restart_at is not UNSET:
            field_dict["restart_at"] = restart_at

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        killed_at = datetime.datetime.fromisoformat(d.pop("killed_at"))

        kills = d.pop("kills")

        _restart_at = d.pop("restart_at", UNSET)
        restart_at: datetime.datetime | Unset
        if isinstance(_restart_at, Unset):
            restart_at = UNSET
        else:
            restart_at = datetime.datetime.fromisoformat(_restart_at)

        oom = cls(
            killed_at=killed_at,
            kills=kills,
            restart_at=restart_at,
        )

        return oom
