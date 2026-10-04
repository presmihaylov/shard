from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.error_object import ErrorObject
    from ..models.event import Event
    from ..models.sandbox import Sandbox


T = TypeVar("T", bound="CreateLine")


@_attrs_define
class CreateLine:
    error: ErrorObject | Unset = UNSET
    event: Event | Unset = UNSET
    sandbox: Sandbox | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        from ..models.error_object import ErrorObject  # noqa: PLC0415
        from ..models.event import Event  # noqa: PLC0415
        from ..models.sandbox import Sandbox  # noqa: PLC0415

        error: dict[str, Any] | Unset = UNSET
        if not isinstance(self.error, Unset):
            error = self.error.to_dict()

        event: dict[str, Any] | Unset = UNSET
        if not isinstance(self.event, Unset):
            event = self.event.to_dict()

        sandbox: dict[str, Any] | Unset = UNSET
        if not isinstance(self.sandbox, Unset):
            sandbox = self.sandbox.to_dict()

        field_dict: dict[str, Any] = {}

        field_dict.update({})
        if error is not UNSET:
            field_dict["error"] = error
        if event is not UNSET:
            field_dict["event"] = event
        if sandbox is not UNSET:
            field_dict["sandbox"] = sandbox

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.error_object import ErrorObject  # noqa: PLC0415
        from ..models.event import Event  # noqa: PLC0415
        from ..models.sandbox import Sandbox  # noqa: PLC0415

        d = dict(src_dict)
        _error = d.pop("error", UNSET)
        error: ErrorObject | Unset
        if isinstance(_error, Unset):
            error = UNSET
        else:
            error = ErrorObject.from_dict(_error)

        _event = d.pop("event", UNSET)
        event: Event | Unset
        if isinstance(_event, Unset):
            event = UNSET
        else:
            event = Event.from_dict(_event)

        _sandbox = d.pop("sandbox", UNSET)
        sandbox: Sandbox | Unset
        if isinstance(_sandbox, Unset):
            sandbox = UNSET
        else:
            sandbox = Sandbox.from_dict(_sandbox)

        create_line = cls(
            error=error,
            event=event,
            sandbox=sandbox,
        )

        return create_line
