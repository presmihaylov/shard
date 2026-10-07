from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.process_status import ProcessStatus
    from ..models.restart_spec import RestartSpec


T = TypeVar("T", bound="Process")


@_attrs_define
class Process:
    command: list[str]
    name: str
    restart: RestartSpec
    status: ProcessStatus
    env: list[str] | Unset = UNSET
    killed: bool | Unset = UNSET
    user: str | Unset = UNSET
    workdir: str | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        from ..models.process_status import ProcessStatus  # noqa: PLC0415
        from ..models.restart_spec import RestartSpec  # noqa: PLC0415

        command = self.command

        name = self.name

        restart = self.restart.to_dict()

        status = self.status.to_dict()

        env: list[str] | Unset = UNSET
        if not isinstance(self.env, Unset):
            env = self.env

        killed = self.killed

        user = self.user

        workdir = self.workdir

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "command": command,
                "name": name,
                "restart": restart,
                "status": status,
            }
        )
        if env is not UNSET:
            field_dict["env"] = env
        if killed is not UNSET:
            field_dict["killed"] = killed
        if user is not UNSET:
            field_dict["user"] = user
        if workdir is not UNSET:
            field_dict["workdir"] = workdir

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.process_status import ProcessStatus  # noqa: PLC0415
        from ..models.restart_spec import RestartSpec  # noqa: PLC0415

        d = dict(src_dict)
        command = cast(list[str], d.pop("command"))

        name = d.pop("name")

        restart = RestartSpec.from_dict(d.pop("restart"))

        status = ProcessStatus.from_dict(d.pop("status"))

        env = cast(list[str], d.pop("env", UNSET))

        killed = d.pop("killed", UNSET)

        user = d.pop("user", UNSET)

        workdir = d.pop("workdir", UNSET)

        process = cls(
            command=command,
            name=name,
            restart=restart,
            status=status,
            env=env,
            killed=killed,
            user=user,
            workdir=workdir,
        )

        return process
