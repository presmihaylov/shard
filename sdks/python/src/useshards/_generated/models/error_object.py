from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="ErrorObject")


@_attrs_define
class ErrorObject:
    code: str
    message: str
    holders: list[str] | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        code = self.code

        message = self.message

        holders: list[str] | Unset = UNSET
        if not isinstance(self.holders, Unset):
            holders = self.holders

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "code": code,
                "message": message,
            }
        )
        if holders is not UNSET:
            field_dict["holders"] = holders

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        code = d.pop("code")

        message = d.pop("message")

        holders = cast(list[str], d.pop("holders", UNSET))

        error_object = cls(
            code=code,
            message=message,
            holders=holders,
        )

        return error_object
