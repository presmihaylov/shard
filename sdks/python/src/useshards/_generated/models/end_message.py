from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.end_message_reason import EndMessageReason
from ..types import UNSET, Unset

T = TypeVar("T", bound="EndMessage")


@_attrs_define
class EndMessage:
    reason: EndMessageReason

    def to_dict(self) -> dict[str, Any]:
        reason = self.reason.value

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "reason": reason,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        reason = EndMessageReason(d.pop("reason"))

        end_message = cls(
            reason=reason,
        )

        return end_message
