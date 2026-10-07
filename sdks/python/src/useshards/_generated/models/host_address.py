from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="HostAddress")


@_attrs_define
class HostAddress:
    address: str
    interface: str

    def to_dict(self) -> dict[str, Any]:
        address = self.address

        interface = self.interface

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "address": address,
                "interface": interface,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        address = d.pop("address")

        interface = d.pop("interface")

        host_address = cls(
            address=address,
            interface=interface,
        )

        return host_address
