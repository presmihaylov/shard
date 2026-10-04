from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="ExitMessage")


@_attrs_define
class ExitMessage:
    code: int
    signal: int
    error: str | Unset = UNSET
    lost_bytes: int | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        code = self.code

        signal = self.signal

        error = self.error

        lost_bytes = self.lost_bytes

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "code": code,
                "signal": signal,
            }
        )
        if error is not UNSET:
            field_dict["error"] = error
        if lost_bytes is not UNSET:
            field_dict["lost_bytes"] = lost_bytes

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        code = d.pop("code")

        signal = d.pop("signal")

        error = d.pop("error", UNSET)

        lost_bytes = d.pop("lost_bytes", UNSET)

        exit_message = cls(
            code=code,
            signal=signal,
            error=error,
            lost_bytes=lost_bytes,
        )

        return exit_message
