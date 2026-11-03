from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="Capabilities")


@_attrs_define
class Capabilities:
    create: bool
    fork: bool
    pause: bool
    port: bool
    remove: bool
    resume: bool
    snapshot: bool
    start: bool
    stop: bool
    swap: bool

    def to_dict(self) -> dict[str, Any]:
        create = self.create

        fork = self.fork

        pause = self.pause

        port = self.port

        remove = self.remove

        resume = self.resume

        snapshot = self.snapshot

        start = self.start

        stop = self.stop

        swap = self.swap

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "create": create,
                "fork": fork,
                "pause": pause,
                "port": port,
                "remove": remove,
                "resume": resume,
                "snapshot": snapshot,
                "start": start,
                "stop": stop,
                "swap": swap,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        create = d.pop("create")

        fork = d.pop("fork")

        pause = d.pop("pause")

        port = d.pop("port")

        remove = d.pop("remove")

        resume = d.pop("resume")

        snapshot = d.pop("snapshot")

        start = d.pop("start")

        stop = d.pop("stop")

        swap = d.pop("swap")

        capabilities = cls(
            create=create,
            fork=fork,
            pause=pause,
            port=port,
            remove=remove,
            resume=resume,
            snapshot=snapshot,
            start=start,
            stop=stop,
            swap=swap,
        )

        return capabilities
