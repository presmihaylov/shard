from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.port import Port


T = TypeVar("T", bound="PortsResponse")


@_attrs_define
class PortsResponse:
    next_: None | str
    ports: list[Port]

    def to_dict(self) -> dict[str, Any]:
        from ..models.port import Port  # noqa: PLC0415

        next_: None | str
        next_ = self.next_

        ports = []
        for ports_item_data in self.ports:
            ports_item = ports_item_data.to_dict()
            ports.append(ports_item)

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "next": next_,
                "ports": ports,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.port import Port  # noqa: PLC0415

        d = dict(src_dict)

        def _parse_next_(data: object) -> None | str:
            if data is None:
                return data
            return cast(None | str, data)

        next_ = _parse_next_(d.pop("next"))

        ports = []
        _ports = d.pop("ports")
        for ports_item_data in _ports:
            ports_item = Port.from_dict(ports_item_data)

            ports.append(ports_item)

        ports_response = cls(
            next_=next_,
            ports=ports,
        )

        return ports_response
