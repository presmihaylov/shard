from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.restart_spec import RestartSpec


T = TypeVar("T", bound="RunRequest")


@_attrs_define
class RunRequest:
    command: list[str]
    env: list[str] | Unset = UNSET
    name: str | Unset = UNSET
    restart: RestartSpec | Unset = UNSET
    user: str | Unset = UNSET
    workdir: str | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        from ..models.restart_spec import RestartSpec  # noqa: PLC0415

        command = self.command

        env: list[str] | Unset = UNSET
        if not isinstance(self.env, Unset):
            env = self.env

        name = self.name

        restart: dict[str, Any] | Unset = UNSET
        if not isinstance(self.restart, Unset):
            restart = self.restart.to_dict()

        user = self.user

        workdir = self.workdir

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "command": command,
            }
        )
        if env is not UNSET:
            field_dict["env"] = env
        if name is not UNSET:
            field_dict["name"] = name
        if restart is not UNSET:
            field_dict["restart"] = restart
        if user is not UNSET:
            field_dict["user"] = user
        if workdir is not UNSET:
            field_dict["workdir"] = workdir

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.restart_spec import RestartSpec  # noqa: PLC0415

        d = dict(src_dict)
        command = cast(list[str], d.pop("command"))

        env = cast(list[str], d.pop("env", UNSET))

        name = d.pop("name", UNSET)

        _restart = d.pop("restart", UNSET)
        restart: RestartSpec | Unset
        if isinstance(_restart, Unset):
            restart = UNSET
        else:
            restart = RestartSpec.from_dict(_restart)

        user = d.pop("user", UNSET)

        workdir = d.pop("workdir", UNSET)

        run_request = cls(
            command=command,
            env=env,
            name=name,
            restart=restart,
            user=user,
            workdir=workdir,
        )

        return run_request
