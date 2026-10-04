from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="SnapshotRequest")


@_attrs_define
class SnapshotRequest:
    sandbox: str
    name: str | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        sandbox = self.sandbox

        name = self.name

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "sandbox": sandbox,
            }
        )
        if name is not UNSET:
            field_dict["name"] = name

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        sandbox = d.pop("sandbox")

        name = d.pop("name", UNSET)

        snapshot_request = cls(
            sandbox=sandbox,
            name=name,
        )

        return snapshot_request
