from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="AppStopRequest")


@_attrs_define
class AppStopRequest:
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

        app_stop_request = cls(
            force=force,
        )

        return app_stop_request
