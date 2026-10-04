from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.exec_ import Exec


T = TypeVar("T", bound="ExecsResponse")


@_attrs_define
class ExecsResponse:
    execs: list[Exec]
    next_: None | str

    def to_dict(self) -> dict[str, Any]:
        from ..models.exec_ import Exec  # noqa: PLC0415

        execs = []
        for execs_item_data in self.execs:
            execs_item = execs_item_data.to_dict()
            execs.append(execs_item)

        next_: None | str
        next_ = self.next_

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "execs": execs,
                "next": next_,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.exec_ import Exec  # noqa: PLC0415

        d = dict(src_dict)
        execs = []
        _execs = d.pop("execs")
        for execs_item_data in _execs:
            execs_item = Exec.from_dict(execs_item_data)

            execs.append(execs_item)

        def _parse_next_(data: object) -> None | str:
            if data is None:
                return data
            return cast(None | str, data)

        next_ = _parse_next_(d.pop("next"))

        execs_response = cls(
            execs=execs,
            next_=next_,
        )

        return execs_response
