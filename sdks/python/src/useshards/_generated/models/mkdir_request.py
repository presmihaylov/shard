from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="MkdirRequest")


@_attrs_define
class MkdirRequest:
    path: str
    mode: str | Unset = UNSET
    parents: bool | Unset = UNSET
    user: str | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        path = self.path

        mode = self.mode

        parents = self.parents

        user = self.user

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "path": path,
            }
        )
        if mode is not UNSET:
            field_dict["mode"] = mode
        if parents is not UNSET:
            field_dict["parents"] = parents
        if user is not UNSET:
            field_dict["user"] = user

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        path = d.pop("path")

        mode = d.pop("mode", UNSET)

        parents = d.pop("parents", UNSET)

        user = d.pop("user", UNSET)

        mkdir_request = cls(
            path=path,
            mode=mode,
            parents=parents,
            user=user,
        )

        return mkdir_request
