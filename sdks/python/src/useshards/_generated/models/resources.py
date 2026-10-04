from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="Resources")


@_attrs_define
class Resources:
    disk_mib: int
    memory_mib: int
    vcpus: int

    def to_dict(self) -> dict[str, Any]:
        disk_mib = self.disk_mib

        memory_mib = self.memory_mib

        vcpus = self.vcpus

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "disk_mib": disk_mib,
                "memory_mib": memory_mib,
                "vcpus": vcpus,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        disk_mib = d.pop("disk_mib")

        memory_mib = d.pop("memory_mib")

        vcpus = d.pop("vcpus")

        resources = cls(
            disk_mib=disk_mib,
            memory_mib=memory_mib,
            vcpus=vcpus,
        )

        return resources
