from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.restart_spec_policy import RestartSpecPolicy
from ..types import UNSET, Unset

T = TypeVar("T", bound="RestartSpec")


@_attrs_define
class RestartSpec:
    policy: RestartSpecPolicy
    backoff: int | Unset = UNSET
    retries: int | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        policy = self.policy.value

        backoff = self.backoff

        retries = self.retries

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "policy": policy,
            }
        )
        if backoff is not UNSET:
            field_dict["backoff"] = backoff
        if retries is not UNSET:
            field_dict["retries"] = retries

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        policy = RestartSpecPolicy(d.pop("policy"))

        backoff = d.pop("backoff", UNSET)

        retries = d.pop("retries", UNSET)

        restart_spec = cls(
            policy=policy,
            backoff=backoff,
            retries=retries,
        )

        return restart_spec
