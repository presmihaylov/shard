from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="AppExit")


@_attrs_define
class AppExit:
    code: int
    restarts: int
    signal: int

    def to_dict(self) -> dict[str, Any]:
        code = self.code

        restarts = self.restarts

        signal = self.signal

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "code": code,
                "restarts": restarts,
                "signal": signal,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        code = d.pop("code")

        restarts = d.pop("restarts")

        signal = d.pop("signal")

        app_exit = cls(
            code=code,
            restarts=restarts,
            signal=signal,
        )

        return app_exit
