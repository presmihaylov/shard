from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="PortForward")


@_attrs_define
class PortForward:
    guest_port: int
    host_port: int
    public: bool | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        guest_port = self.guest_port

        host_port = self.host_port

        public = self.public

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "guest_port": guest_port,
                "host_port": host_port,
            }
        )
        if public is not UNSET:
            field_dict["public"] = public

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        guest_port = d.pop("guest_port")

        host_port = d.pop("host_port")

        public = d.pop("public", UNSET)

        port_forward = cls(
            guest_port=guest_port,
            host_port=host_port,
            public=public,
        )

        return port_forward
