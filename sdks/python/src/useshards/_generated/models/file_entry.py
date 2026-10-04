from __future__ import annotations

import datetime
from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="FileEntry")


@_attrs_define
class FileEntry:
    gid: int
    mode: int
    mtime: datetime.datetime
    name: str
    size: int
    type_: str
    uid: int

    def to_dict(self) -> dict[str, Any]:
        gid = self.gid

        mode = self.mode

        mtime = self.mtime.isoformat()

        name = self.name

        size = self.size

        type_ = self.type_

        uid = self.uid

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "gid": gid,
                "mode": mode,
                "mtime": mtime,
                "name": name,
                "size": size,
                "type": type_,
                "uid": uid,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        gid = d.pop("gid")

        mode = d.pop("mode")

        mtime = datetime.datetime.fromisoformat(d.pop("mtime"))

        name = d.pop("name")

        size = d.pop("size")

        type_ = d.pop("type")

        uid = d.pop("uid")

        file_entry = cls(
            gid=gid,
            mode=mode,
            mtime=mtime,
            name=name,
            size=size,
            type_=type_,
            uid=uid,
        )

        return file_entry
