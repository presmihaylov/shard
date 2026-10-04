from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="KillRequest")


@_attrs_define
class KillRequest:
    signal: str | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        signal = self.signal

        field_dict: dict[str, Any] = {}

        field_dict.update({})
        if signal is not UNSET:
            field_dict["signal"] = signal

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        signal = d.pop("signal", UNSET)

        kill_request = cls(
            signal=signal,
        )

        return kill_request
