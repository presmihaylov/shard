from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.rule import Rule


T = TypeVar("T", bound="Policy")


@_attrs_define
class Policy:
    name: str
    rules: list[Rule]

    def to_dict(self) -> dict[str, Any]:
        from ..models.rule import Rule  # noqa: PLC0415

        name = self.name

        rules = []
        for rules_item_data in self.rules:
            rules_item = rules_item_data.to_dict()
            rules.append(rules_item)

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "name": name,
                "rules": rules,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.rule import Rule  # noqa: PLC0415

        d = dict(src_dict)
        name = d.pop("name")

        rules = []
        _rules = d.pop("rules")
        for rules_item_data in _rules:
            rules_item = Rule.from_dict(rules_item_data)

            rules.append(rules_item)

        policy = cls(
            name=name,
            rules=rules,
        )

        return policy
