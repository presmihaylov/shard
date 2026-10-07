from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.host_address import HostAddress


T = TypeVar("T", bound="Port")


@_attrs_define
class Port:
    address: str
    guest_port: int
    host_port: int
    listening: bool
    public: bool
    reachable_on: list[HostAddress]
    sandbox: str
    error: str | Unset = UNSET
    sandbox_name: str | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        from ..models.host_address import HostAddress  # noqa: PLC0415

        address = self.address

        guest_port = self.guest_port

        host_port = self.host_port

        listening = self.listening

        public = self.public

        reachable_on = []
        for reachable_on_item_data in self.reachable_on:
            reachable_on_item = reachable_on_item_data.to_dict()
            reachable_on.append(reachable_on_item)

        sandbox = self.sandbox

        error = self.error

        sandbox_name = self.sandbox_name

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "address": address,
                "guest_port": guest_port,
                "host_port": host_port,
                "listening": listening,
                "public": public,
                "reachable_on": reachable_on,
                "sandbox": sandbox,
            }
        )
        if error is not UNSET:
            field_dict["error"] = error
        if sandbox_name is not UNSET:
            field_dict["sandbox_name"] = sandbox_name

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.host_address import HostAddress  # noqa: PLC0415

        d = dict(src_dict)
        address = d.pop("address")

        guest_port = d.pop("guest_port")

        host_port = d.pop("host_port")

        listening = d.pop("listening")

        public = d.pop("public")

        reachable_on = []
        _reachable_on = d.pop("reachable_on")
        for reachable_on_item_data in _reachable_on:
            reachable_on_item = HostAddress.from_dict(reachable_on_item_data)

            reachable_on.append(reachable_on_item)

        sandbox = d.pop("sandbox")

        error = d.pop("error", UNSET)

        sandbox_name = d.pop("sandbox_name", UNSET)

        port = cls(
            address=address,
            guest_port=guest_port,
            host_port=host_port,
            listening=listening,
            public=public,
            reachable_on=reachable_on,
            sandbox=sandbox,
            error=error,
            sandbox_name=sandbox_name,
        )

        return port
