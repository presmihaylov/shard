from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.terminal_size import TerminalSize


T = TypeVar("T", bound="ExecRequest")


@_attrs_define
class ExecRequest:
    command: list[str]
    attach: bool | Unset = UNSET
    env: list[str] | Unset = UNSET
    size: TerminalSize | Unset = UNSET
    stdin: bool | Unset = UNSET
    tty: bool | Unset = UNSET
    user: str | Unset = UNSET
    workdir: str | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        from ..models.terminal_size import TerminalSize  # noqa: PLC0415

        command = self.command

        attach = self.attach

        env: list[str] | Unset = UNSET
        if not isinstance(self.env, Unset):
            env = self.env

        size: dict[str, Any] | Unset = UNSET
        if not isinstance(self.size, Unset):
            size = self.size.to_dict()

        stdin = self.stdin

        tty = self.tty

        user = self.user

        workdir = self.workdir

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "command": command,
            }
        )
        if attach is not UNSET:
            field_dict["attach"] = attach
        if env is not UNSET:
            field_dict["env"] = env
        if size is not UNSET:
            field_dict["size"] = size
        if stdin is not UNSET:
            field_dict["stdin"] = stdin
        if tty is not UNSET:
            field_dict["tty"] = tty
        if user is not UNSET:
            field_dict["user"] = user
        if workdir is not UNSET:
            field_dict["workdir"] = workdir

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.terminal_size import TerminalSize  # noqa: PLC0415

        d = dict(src_dict)
        command = cast(list[str], d.pop("command"))

        attach = d.pop("attach", UNSET)

        env = cast(list[str], d.pop("env", UNSET))

        _size = d.pop("size", UNSET)
        size: TerminalSize | Unset
        if isinstance(_size, Unset):
            size = UNSET
        else:
            size = TerminalSize.from_dict(_size)

        stdin = d.pop("stdin", UNSET)

        tty = d.pop("tty", UNSET)

        user = d.pop("user", UNSET)

        workdir = d.pop("workdir", UNSET)

        exec_request = cls(
            command=command,
            attach=attach,
            env=env,
            size=size,
            stdin=stdin,
            tty=tty,
            user=user,
            workdir=workdir,
        )

        return exec_request
