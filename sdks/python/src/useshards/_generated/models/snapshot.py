from __future__ import annotations

import datetime
from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="Snapshot")


@_attrs_define
class Snapshot:
    created_at: datetime.datetime
    digest: str
    disk_mib: int
    id: str
    image: str
    memory_mib: int
    provider: str
    size: int
    source: str
    name: str | Unset = UNSET
    source_name: str | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        created_at = self.created_at.isoformat()

        digest = self.digest

        disk_mib = self.disk_mib

        id = self.id

        image = self.image

        memory_mib = self.memory_mib

        provider = self.provider

        size = self.size

        source = self.source

        name = self.name

        source_name = self.source_name

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "created_at": created_at,
                "digest": digest,
                "disk_mib": disk_mib,
                "id": id,
                "image": image,
                "memory_mib": memory_mib,
                "provider": provider,
                "size": size,
                "source": source,
            }
        )
        if name is not UNSET:
            field_dict["name"] = name
        if source_name is not UNSET:
            field_dict["source_name"] = source_name

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        created_at = datetime.datetime.fromisoformat(d.pop("created_at"))

        digest = d.pop("digest")

        disk_mib = d.pop("disk_mib")

        id = d.pop("id")

        image = d.pop("image")

        memory_mib = d.pop("memory_mib")

        provider = d.pop("provider")

        size = d.pop("size")

        source = d.pop("source")

        name = d.pop("name", UNSET)

        source_name = d.pop("source_name", UNSET)

        snapshot = cls(
            created_at=created_at,
            digest=digest,
            disk_mib=disk_mib,
            id=id,
            image=image,
            memory_mib=memory_mib,
            provider=provider,
            size=size,
            source=source,
            name=name,
            source_name=source_name,
        )

        return snapshot
