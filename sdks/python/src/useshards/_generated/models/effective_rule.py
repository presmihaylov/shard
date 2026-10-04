from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.effective_rule_action import EffectiveRuleAction
from ..models.effective_rule_implied import EffectiveRuleImplied
from ..models.effective_rule_protocol import EffectiveRuleProtocol
from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.destination import Destination


T = TypeVar("T", bound="EffectiveRule")


@_attrs_define
class EffectiveRule:
    action: EffectiveRuleAction
    destination: Destination
    id: str
    implied: EffectiveRuleImplied | Unset = UNSET
    ports: list[int] | Unset = UNSET
    protocol: EffectiveRuleProtocol | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        from ..models.destination import Destination  # noqa: PLC0415

        action = self.action.value

        destination = self.destination.to_dict()

        id = self.id

        implied: str | Unset = UNSET
        if not isinstance(self.implied, Unset):
            implied = self.implied.value

        ports: list[int] | Unset = UNSET
        if not isinstance(self.ports, Unset):
            ports = self.ports

        protocol: str | Unset = UNSET
        if not isinstance(self.protocol, Unset):
            protocol = self.protocol.value

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "action": action,
                "destination": destination,
                "id": id,
            }
        )
        if implied is not UNSET:
            field_dict["implied"] = implied
        if ports is not UNSET:
            field_dict["ports"] = ports
        if protocol is not UNSET:
            field_dict["protocol"] = protocol

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.destination import Destination  # noqa: PLC0415

        d = dict(src_dict)
        action = EffectiveRuleAction(d.pop("action"))

        destination = Destination.from_dict(d.pop("destination"))

        id = d.pop("id")

        _implied = d.pop("implied", UNSET)
        implied: EffectiveRuleImplied | Unset
        if isinstance(_implied, Unset):
            implied = UNSET
        else:
            implied = EffectiveRuleImplied(_implied)

        ports = cast(list[int], d.pop("ports", UNSET))

        _protocol = d.pop("protocol", UNSET)
        protocol: EffectiveRuleProtocol | Unset
        if isinstance(_protocol, Unset):
            protocol = UNSET
        else:
            protocol = EffectiveRuleProtocol(_protocol)

        effective_rule = cls(
            action=action,
            destination=destination,
            id=id,
            implied=implied,
            ports=ports,
            protocol=protocol,
        )

        return effective_rule
