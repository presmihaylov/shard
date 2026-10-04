from __future__ import annotations

import datetime
from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.egress_decision_source import EgressDecisionSource
from ..models.egress_decision_verdict import EgressDecisionVerdict
from ..types import UNSET, Unset

T = TypeVar("T", bound="EgressDecision")


@_attrs_define
class EgressDecision:
    rule: str
    source: EgressDecisionSource
    time: datetime.datetime
    verdict: EgressDecisionVerdict
    address: str | Unset = UNSET
    host: str | Unset = UNSET
    port: int | Unset = UNSET
    reason: str | Unset = UNSET
    rule_text: str | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        rule = self.rule

        source = self.source.value

        time = self.time.isoformat()

        verdict = self.verdict.value

        address = self.address

        host = self.host

        port = self.port

        reason = self.reason

        rule_text = self.rule_text

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "rule": rule,
                "source": source,
                "time": time,
                "verdict": verdict,
            }
        )
        if address is not UNSET:
            field_dict["address"] = address
        if host is not UNSET:
            field_dict["host"] = host
        if port is not UNSET:
            field_dict["port"] = port
        if reason is not UNSET:
            field_dict["reason"] = reason
        if rule_text is not UNSET:
            field_dict["rule_text"] = rule_text

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        rule = d.pop("rule")

        source = EgressDecisionSource(d.pop("source"))

        time = datetime.datetime.fromisoformat(d.pop("time"))

        verdict = EgressDecisionVerdict(d.pop("verdict"))

        address = d.pop("address", UNSET)

        host = d.pop("host", UNSET)

        port = d.pop("port", UNSET)

        reason = d.pop("reason", UNSET)

        rule_text = d.pop("rule_text", UNSET)

        egress_decision = cls(
            rule=rule,
            source=source,
            time=time,
            verdict=verdict,
            address=address,
            host=host,
            port=port,
            reason=reason,
            rule_text=rule_text,
        )

        return egress_decision
