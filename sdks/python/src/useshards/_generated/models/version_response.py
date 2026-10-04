from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="VersionResponse")


@_attrs_define
class VersionResponse:
    api_version: str
    version: str

    def to_dict(self) -> dict[str, Any]:
        api_version = self.api_version

        version = self.version

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "api_version": api_version,
                "version": version,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        api_version = d.pop("api_version")

        version = d.pop("version")

        version_response = cls(
            api_version=api_version,
            version=version,
        )

        return version_response
