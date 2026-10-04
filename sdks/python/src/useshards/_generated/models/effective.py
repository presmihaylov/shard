from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.effective_rule import EffectiveRule


T = TypeVar("T", bound="Effective")


@_attrs_define
class Effective:
    rules: list[EffectiveRule]
    missing: bool | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        from ..models.effective_rule import EffectiveRule  # noqa: PLC0415

        rules = []
        for rules_item_data in self.rules:
            rules_item = rules_item_data.to_dict()
            rules.append(rules_item)

        missing = self.missing

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "rules": rules,
            }
        )
        if missing is not UNSET:
            field_dict["missing"] = missing

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.effective_rule import EffectiveRule  # noqa: PLC0415

        d = dict(src_dict)
        rules = []
        _rules = d.pop("rules")
        for rules_item_data in _rules:
            rules_item = EffectiveRule.from_dict(rules_item_data)

            rules.append(rules_item)

        missing = d.pop("missing", UNSET)

        effective = cls(
            rules=rules,
            missing=missing,
        )

        return effective
