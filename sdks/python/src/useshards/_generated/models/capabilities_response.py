from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="CapabilitiesResponse")


@_attrs_define
class CapabilitiesResponse:
    """
    Attributes:
        provider (str):
        unsupported (list[str]):
    """

    provider: str
    unsupported: list[str]

    def to_dict(self) -> dict[str, Any]:
        provider = self.provider

        unsupported = self.unsupported

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "provider": provider,
                "unsupported": unsupported,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        provider = d.pop("provider")

        unsupported = cast(list[str], d.pop("unsupported"))

        capabilities_response = cls(
            provider=provider,
            unsupported=unsupported,
        )

        return capabilities_response
