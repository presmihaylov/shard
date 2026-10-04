from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="TerminalSize")


@_attrs_define
class TerminalSize:
    cols: int | Unset = UNSET
    rows: int | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        cols = self.cols

        rows = self.rows

        field_dict: dict[str, Any] = {}

        field_dict.update({})
        if cols is not UNSET:
            field_dict["cols"] = cols
        if rows is not UNSET:
            field_dict["rows"] = rows

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        cols = d.pop("cols", UNSET)

        rows = d.pop("rows", UNSET)

        terminal_size = cls(
            cols=cols,
            rows=rows,
        )

        return terminal_size
