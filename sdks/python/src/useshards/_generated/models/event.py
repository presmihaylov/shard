from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.event_status import EventStatus
from ..types import UNSET, Unset

T = TypeVar("T", bound="Event")


@_attrs_define
class Event:
    status: EventStatus
    bytes_: int | Unset = UNSET
    digest: str | Unset = UNSET
    layer: int | Unset = UNSET
    layers: int | Unset = UNSET
    present: bool | Unset = UNSET
    reference: str | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        status = self.status.value

        bytes_ = self.bytes_

        digest = self.digest

        layer = self.layer

        layers = self.layers

        present = self.present

        reference = self.reference

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "status": status,
            }
        )
        if bytes_ is not UNSET:
            field_dict["bytes"] = bytes_
        if digest is not UNSET:
            field_dict["digest"] = digest
        if layer is not UNSET:
            field_dict["layer"] = layer
        if layers is not UNSET:
            field_dict["layers"] = layers
        if present is not UNSET:
            field_dict["present"] = present
        if reference is not UNSET:
            field_dict["reference"] = reference

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        status = EventStatus(d.pop("status"))

        bytes_ = d.pop("bytes", UNSET)

        digest = d.pop("digest", UNSET)

        layer = d.pop("layer", UNSET)

        layers = d.pop("layers", UNSET)

        present = d.pop("present", UNSET)

        reference = d.pop("reference", UNSET)

        event = cls(
            status=status,
            bytes_=bytes_,
            digest=digest,
            layer=layer,
            layers=layers,
            present=present,
            reference=reference,
        )

        return event
