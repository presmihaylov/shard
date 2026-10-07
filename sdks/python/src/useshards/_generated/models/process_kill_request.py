from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="ProcessKillRequest")


@_attrs_define
class ProcessKillRequest:
    force: bool | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        force = self.force

        field_dict: dict[str, Any] = {}

        field_dict.update({})
        if force is not UNSET:
            field_dict["force"] = force

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        force = d.pop("force", UNSET)

        process_kill_request = cls(
            force=force,
        )

        return process_kill_request
