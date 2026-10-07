from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.process import Process


T = TypeVar("T", bound="ProcessesResponse")


@_attrs_define
class ProcessesResponse:
    processes: list[Process]

    def to_dict(self) -> dict[str, Any]:
        from ..models.process import Process  # noqa: PLC0415

        processes = []
        for processes_item_data in self.processes:
            processes_item = processes_item_data.to_dict()
            processes.append(processes_item)

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "processes": processes,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.process import Process  # noqa: PLC0415

        d = dict(src_dict)
        processes = []
        _processes = d.pop("processes")
        for processes_item_data in _processes:
            processes_item = Process.from_dict(processes_item_data)

            processes.append(processes_item)

        processes_response = cls(
            processes=processes,
        )

        return processes_response
