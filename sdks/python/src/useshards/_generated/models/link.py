from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="Link")


@_attrs_define
class Link:
    address: str
    target: str
    target_id: str

    def to_dict(self) -> dict[str, Any]:
        address = self.address

        target = self.target

        target_id = self.target_id

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "address": address,
                "target": target,
                "target_id": target_id,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        address = d.pop("address")

        target = d.pop("target")

        target_id = d.pop("target_id")

        link = cls(
            address=address,
            target=target,
            target_id=target_id,
        )

        return link
