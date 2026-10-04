from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.snapshot import Snapshot


T = TypeVar("T", bound="SnapshotsResponse")


@_attrs_define
class SnapshotsResponse:
    next_: None | str
    snapshots: list[Snapshot]

    def to_dict(self) -> dict[str, Any]:
        from ..models.snapshot import Snapshot  # noqa: PLC0415

        next_: None | str
        next_ = self.next_

        snapshots = []
        for snapshots_item_data in self.snapshots:
            snapshots_item = snapshots_item_data.to_dict()
            snapshots.append(snapshots_item)

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "next": next_,
                "snapshots": snapshots,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.snapshot import Snapshot  # noqa: PLC0415

        d = dict(src_dict)

        def _parse_next_(data: object) -> None | str:
            if data is None:
                return data
            return cast(None | str, data)

        next_ = _parse_next_(d.pop("next"))

        snapshots = []
        _snapshots = d.pop("snapshots")
        for snapshots_item_data in _snapshots:
            snapshots_item = Snapshot.from_dict(snapshots_item_data)

            snapshots.append(snapshots_item)

        snapshots_response = cls(
            next_=next_,
            snapshots=snapshots,
        )

        return snapshots_response
