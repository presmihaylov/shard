from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.policy import Policy


T = TypeVar("T", bound="PoliciesResponse")


@_attrs_define
class PoliciesResponse:
    next_: None | str
    policies: list[Policy]

    def to_dict(self) -> dict[str, Any]:
        from ..models.policy import Policy  # noqa: PLC0415

        next_: None | str
        next_ = self.next_

        policies = []
        for policies_item_data in self.policies:
            policies_item = policies_item_data.to_dict()
            policies.append(policies_item)

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "next": next_,
                "policies": policies,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.policy import Policy  # noqa: PLC0415

        d = dict(src_dict)

        def _parse_next_(data: object) -> None | str:
            if data is None:
                return data
            return cast(None | str, data)

        next_ = _parse_next_(d.pop("next"))

        policies = []
        _policies = d.pop("policies")
        for policies_item_data in _policies:
            policies_item = Policy.from_dict(policies_item_data)

            policies.append(policies_item)

        policies_response = cls(
            next_=next_,
            policies=policies,
        )

        return policies_response
