from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="PolicyAttachRequest")


@_attrs_define
class PolicyAttachRequest:
    policy: str

    def to_dict(self) -> dict[str, Any]:
        policy = self.policy

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "policy": policy,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        policy = d.pop("policy")

        policy_attach_request = cls(
            policy=policy,
        )

        return policy_attach_request
