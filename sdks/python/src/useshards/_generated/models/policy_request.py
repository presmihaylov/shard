from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.rule_text import RuleText


T = TypeVar("T", bound="PolicyRequest")


@_attrs_define
class PolicyRequest:
    rules: list[RuleText] | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        from ..models.rule_text import RuleText  # noqa: PLC0415

        rules: list[dict[str, Any]] | Unset = UNSET
        if not isinstance(self.rules, Unset):
            rules = []
            for rules_item_data in self.rules:
                rules_item = rules_item_data.to_dict()
                rules.append(rules_item)

        field_dict: dict[str, Any] = {}

        field_dict.update({})
        if rules is not UNSET:
            field_dict["rules"] = rules

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.rule_text import RuleText  # noqa: PLC0415

        d = dict(src_dict)
        _rules = d.pop("rules", UNSET)
        rules: list[RuleText] | Unset = UNSET
        if _rules is not UNSET:
            rules = []
            for rules_item_data in _rules:
                rules_item = RuleText.from_dict(rules_item_data)

                rules.append(rules_item)

        policy_request = cls(
            rules=rules,
        )

        return policy_request
