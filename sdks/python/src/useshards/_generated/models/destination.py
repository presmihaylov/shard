from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.destination_kind import DestinationKind
from ..types import UNSET, Unset

T = TypeVar("T", bound="Destination")


@_attrs_define
class Destination:
    kind: DestinationKind
    value: str

    def to_dict(self) -> dict[str, Any]:
        kind = self.kind.value

        value = self.value

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "kind": kind,
                "value": value,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        kind = DestinationKind(d.pop("kind"))

        value = d.pop("value")

        destination = cls(
            kind=kind,
            value=value,
        )

        return destination
