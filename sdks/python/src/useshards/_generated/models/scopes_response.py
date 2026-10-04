from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.scope import Scope


T = TypeVar("T", bound="ScopesResponse")


@_attrs_define
class ScopesResponse:
    scopes: list[Scope]

    def to_dict(self) -> dict[str, Any]:
        from ..models.scope import Scope  # noqa: PLC0415

        scopes = []
        for scopes_item_data in self.scopes:
            scopes_item = scopes_item_data.to_dict()
            scopes.append(scopes_item)

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "scopes": scopes,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.scope import Scope  # noqa: PLC0415

        d = dict(src_dict)
        scopes = []
        _scopes = d.pop("scopes")
        for scopes_item_data in _scopes:
            scopes_item = Scope.from_dict(scopes_item_data)

            scopes.append(scopes_item)

        scopes_response = cls(
            scopes=scopes,
        )

        return scopes_response
