from __future__ import annotations

import datetime
from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.restart_policy import RestartPolicy
from ..types import UNSET, Unset

T = TypeVar("T", bound="Restart")


@_attrs_define
class Restart:
    backoff: int
    count: int
    ended: bool
    gave_up: bool
    policy: RestartPolicy
    last_at: datetime.datetime | Unset = UNSET
    retries: int | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        backoff = self.backoff

        count = self.count

        ended = self.ended

        gave_up = self.gave_up

        policy = self.policy.value

        last_at: str | Unset = UNSET
        if not isinstance(self.last_at, Unset):
            last_at = self.last_at.isoformat()

        retries = self.retries

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "backoff": backoff,
                "count": count,
                "ended": ended,
                "gave_up": gave_up,
                "policy": policy,
            }
        )
        if last_at is not UNSET:
            field_dict["last_at"] = last_at
        if retries is not UNSET:
            field_dict["retries"] = retries

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        backoff = d.pop("backoff")

        count = d.pop("count")

        ended = d.pop("ended")

        gave_up = d.pop("gave_up")

        policy = RestartPolicy(d.pop("policy"))

        _last_at = d.pop("last_at", UNSET)
        last_at: datetime.datetime | Unset
        if isinstance(_last_at, Unset):
            last_at = UNSET
        else:
            last_at = datetime.datetime.fromisoformat(_last_at)

        retries = d.pop("retries", UNSET)

        restart = cls(
            backoff=backoff,
            count=count,
            ended=ended,
            gave_up=gave_up,
            policy=policy,
            last_at=last_at,
            retries=retries,
        )

        return restart
