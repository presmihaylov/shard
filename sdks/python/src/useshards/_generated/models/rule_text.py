from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.rule_text_action import RuleTextAction
from ..types import UNSET, Unset

T = TypeVar("T", bound="RuleText")


@_attrs_define
class RuleText:
    action: RuleTextAction
    rule: str

    def to_dict(self) -> dict[str, Any]:
        action = self.action.value

        rule = self.rule

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "action": action,
                "rule": rule,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        action = RuleTextAction(d.pop("action"))

        rule = d.pop("rule")

        rule_text = cls(
            action=action,
            rule=rule,
        )

        return rule_text
