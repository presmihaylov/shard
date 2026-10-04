from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.sandbox import Sandbox


T = TypeVar("T", bound="SandboxesResponse")


@_attrs_define
class SandboxesResponse:
    next_: None | str
    sandboxes: list[Sandbox]
    warnings: list[str] | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        from ..models.sandbox import Sandbox  # noqa: PLC0415

        next_: None | str
        next_ = self.next_

        sandboxes = []
        for sandboxes_item_data in self.sandboxes:
            sandboxes_item = sandboxes_item_data.to_dict()
            sandboxes.append(sandboxes_item)

        warnings: list[str] | Unset = UNSET
        if not isinstance(self.warnings, Unset):
            warnings = self.warnings

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "next": next_,
                "sandboxes": sandboxes,
            }
        )
        if warnings is not UNSET:
            field_dict["warnings"] = warnings

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.sandbox import Sandbox  # noqa: PLC0415

        d = dict(src_dict)

        def _parse_next_(data: object) -> None | str:
            if data is None:
                return data
            return cast(None | str, data)

        next_ = _parse_next_(d.pop("next"))

        sandboxes = []
        _sandboxes = d.pop("sandboxes")
        for sandboxes_item_data in _sandboxes:
            sandboxes_item = Sandbox.from_dict(sandboxes_item_data)

            sandboxes.append(sandboxes_item)

        warnings = cast(list[str], d.pop("warnings", UNSET))

        sandboxes_response = cls(
            next_=next_,
            sandboxes=sandboxes,
            warnings=warnings,
        )

        return sandboxes_response
