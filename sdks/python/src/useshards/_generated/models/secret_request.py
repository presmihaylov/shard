from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="SecretRequest")


@_attrs_define
class SecretRequest:
    value: str
    destinations: list[str] | Unset = UNSET
    placeholder: str | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        value = self.value

        destinations: list[str] | Unset = UNSET
        if not isinstance(self.destinations, Unset):
            destinations = self.destinations

        placeholder = self.placeholder

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "value": value,
            }
        )
        if destinations is not UNSET:
            field_dict["destinations"] = destinations
        if placeholder is not UNSET:
            field_dict["placeholder"] = placeholder

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        value = d.pop("value")

        destinations = cast(list[str], d.pop("destinations", UNSET))

        placeholder = d.pop("placeholder", UNSET)

        secret_request = cls(
            value=value,
            destinations=destinations,
            placeholder=placeholder,
        )

        return secret_request
