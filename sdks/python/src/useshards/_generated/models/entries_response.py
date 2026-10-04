from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.file_entry import FileEntry


T = TypeVar("T", bound="EntriesResponse")


@_attrs_define
class EntriesResponse:
    entries: list[FileEntry]

    def to_dict(self) -> dict[str, Any]:
        from ..models.file_entry import FileEntry  # noqa: PLC0415

        entries = []
        for entries_item_data in self.entries:
            entries_item = entries_item_data.to_dict()
            entries.append(entries_item)

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "entries": entries,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.file_entry import FileEntry  # noqa: PLC0415

        d = dict(src_dict)
        entries = []
        _entries = d.pop("entries")
        for entries_item_data in _entries:
            entries_item = FileEntry.from_dict(entries_item_data)

            entries.append(entries_item)

        entries_response = cls(
            entries=entries,
        )

        return entries_response
