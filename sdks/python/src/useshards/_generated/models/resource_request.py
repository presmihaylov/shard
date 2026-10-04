from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="ResourceRequest")


@_attrs_define
class ResourceRequest:
    disk_mib: int | Unset = UNSET
    memory_mib: int | Unset = UNSET
    vcpus: int | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        disk_mib = self.disk_mib

        memory_mib = self.memory_mib

        vcpus = self.vcpus

        field_dict: dict[str, Any] = {}

        field_dict.update({})
        if disk_mib is not UNSET:
            field_dict["disk_mib"] = disk_mib
        if memory_mib is not UNSET:
            field_dict["memory_mib"] = memory_mib
        if vcpus is not UNSET:
            field_dict["vcpus"] = vcpus

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        disk_mib = d.pop("disk_mib", UNSET)

        memory_mib = d.pop("memory_mib", UNSET)

        vcpus = d.pop("vcpus", UNSET)

        resource_request = cls(
            disk_mib=disk_mib,
            memory_mib=memory_mib,
            vcpus=vcpus,
        )

        return resource_request
