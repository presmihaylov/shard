from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.secret import Secret


T = TypeVar("T", bound="SecretsResponse")


@_attrs_define
class SecretsResponse:
    next_: None | str
    secrets: list[Secret]
    warnings: list[str] | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        from ..models.secret import Secret  # noqa: PLC0415

        next_: None | str
        next_ = self.next_

        secrets = []
        for secrets_item_data in self.secrets:
            secrets_item = secrets_item_data.to_dict()
            secrets.append(secrets_item)

        warnings: list[str] | Unset = UNSET
        if not isinstance(self.warnings, Unset):
            warnings = self.warnings

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "next": next_,
                "secrets": secrets,
            }
        )
        if warnings is not UNSET:
            field_dict["warnings"] = warnings

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.secret import Secret  # noqa: PLC0415

        d = dict(src_dict)

        def _parse_next_(data: object) -> None | str:
            if data is None:
                return data
            return cast(None | str, data)

        next_ = _parse_next_(d.pop("next"))

        secrets = []
        _secrets = d.pop("secrets")
        for secrets_item_data in _secrets:
            secrets_item = Secret.from_dict(secrets_item_data)

            secrets.append(secrets_item)

        warnings = cast(list[str], d.pop("warnings", UNSET))

        secrets_response = cls(
            next_=next_,
            secrets=secrets,
            warnings=warnings,
        )

        return secrets_response
