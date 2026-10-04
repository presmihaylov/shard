from __future__ import annotations

import datetime
from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="Secret")


@_attrs_define
class Secret:
    destinations: list[str]
    name: str
    placeholder: str
    updated_at: datetime.datetime

    def to_dict(self) -> dict[str, Any]:
        destinations = self.destinations

        name = self.name

        placeholder = self.placeholder

        updated_at = self.updated_at.isoformat()

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "destinations": destinations,
                "name": name,
                "placeholder": placeholder,
                "updated_at": updated_at,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        destinations = cast(list[str], d.pop("destinations"))

        name = d.pop("name")

        placeholder = d.pop("placeholder")

        updated_at = datetime.datetime.fromisoformat(d.pop("updated_at"))

        secret = cls(
            destinations=destinations,
            name=name,
            placeholder=placeholder,
            updated_at=updated_at,
        )

        return secret
