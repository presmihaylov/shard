from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.resize_resources import ResizeResources


T = TypeVar("T", bound="ResizeRequest")


@_attrs_define
class ResizeRequest:
    resources: ResizeResources

    def to_dict(self) -> dict[str, Any]:
        from ..models.resize_resources import ResizeResources  # noqa: PLC0415

        resources = self.resources.to_dict()

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "resources": resources,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.resize_resources import ResizeResources  # noqa: PLC0415

        d = dict(src_dict)
        resources = ResizeResources.from_dict(d.pop("resources"))

        resize_request = cls(
            resources=resources,
        )

        return resize_request
