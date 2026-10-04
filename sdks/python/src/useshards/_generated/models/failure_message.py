from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.failure_error import FailureError


T = TypeVar("T", bound="FailureMessage")


@_attrs_define
class FailureMessage:
    error: FailureError

    def to_dict(self) -> dict[str, Any]:
        from ..models.failure_error import FailureError  # noqa: PLC0415

        error = self.error.to_dict()

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "error": error,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.failure_error import FailureError  # noqa: PLC0415

        d = dict(src_dict)
        error = FailureError.from_dict(d.pop("error"))

        failure_message = cls(
            error=error,
        )

        return failure_message
