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
    exit_code: int | Unset = UNSET
    holders: list[str] | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        code = self.code

        message = self.message

        exit_code = self.exit_code

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
        if exit_code is not UNSET:
            field_dict["exit_code"] = exit_code
        if holders is not UNSET:
            field_dict["holders"] = holders

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        code = d.pop("code")

        message = d.pop("message")

        exit_code = d.pop("exit_code", UNSET)

        holders = cast(list[str], d.pop("holders", UNSET))

        error_object = cls(
            code=code,
            message=message,
            exit_code=exit_code,
            holders=holders,
        )

        return error_object
