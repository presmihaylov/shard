from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.rule_action import RuleAction
from ..models.rule_protocol import RuleProtocol
from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.destination import Destination


T = TypeVar("T", bound="Rule")


@_attrs_define
class Rule:
    action: RuleAction
    destination: Destination
    ports: list[int] | Unset = UNSET
    protocol: RuleProtocol | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        from ..models.destination import Destination  # noqa: PLC0415

        action = self.action.value

        destination = self.destination.to_dict()

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
            }
        )
        if ports is not UNSET:
            field_dict["ports"] = ports
        if protocol is not UNSET:
            field_dict["protocol"] = protocol

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.destination import Destination  # noqa: PLC0415

        d = dict(src_dict)
        action = RuleAction(d.pop("action"))

        destination = Destination.from_dict(d.pop("destination"))

        ports = cast(list[int], d.pop("ports", UNSET))

        _protocol = d.pop("protocol", UNSET)
        protocol: RuleProtocol | Unset
        if isinstance(_protocol, Unset):
            protocol = UNSET
        else:
            protocol = RuleProtocol(_protocol)

        rule = cls(
            action=action,
            destination=destination,
            ports=ports,
            protocol=protocol,
        )

        return rule
