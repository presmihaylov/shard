from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.resource_request import ResourceRequest
    from ..models.restart_spec import RestartSpec


T = TypeVar("T", bound="CreateRequest")


@_attrs_define
class CreateRequest:
    command: list[str] | Unset = UNSET
    env: list[str] | Unset = UNSET
    image: str | Unset = UNSET
    name: str | Unset = UNSET
    policy: str | Unset = UNSET
    resources: ResourceRequest | Unset = UNSET
    restart: RestartSpec | Unset = UNSET
    secrets: list[str] | Unset = UNSET
    snapshot: str | Unset = UNSET
    user: str | Unset = UNSET
    workdir: str | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        from ..models.resource_request import ResourceRequest  # noqa: PLC0415
        from ..models.restart_spec import RestartSpec  # noqa: PLC0415

        command: list[str] | Unset = UNSET
        if not isinstance(self.command, Unset):
            command = self.command

        env: list[str] | Unset = UNSET
        if not isinstance(self.env, Unset):
            env = self.env

        image = self.image

        name = self.name

        policy = self.policy

        resources: dict[str, Any] | Unset = UNSET
        if not isinstance(self.resources, Unset):
            resources = self.resources.to_dict()

        restart: dict[str, Any] | Unset = UNSET
        if not isinstance(self.restart, Unset):
            restart = self.restart.to_dict()

        secrets: list[str] | Unset = UNSET
        if not isinstance(self.secrets, Unset):
            secrets = self.secrets

        snapshot = self.snapshot

        user = self.user

        workdir = self.workdir

        field_dict: dict[str, Any] = {}

        field_dict.update({})
        if command is not UNSET:
            field_dict["command"] = command
        if env is not UNSET:
            field_dict["env"] = env
        if image is not UNSET:
            field_dict["image"] = image
        if name is not UNSET:
            field_dict["name"] = name
        if policy is not UNSET:
            field_dict["policy"] = policy
        if resources is not UNSET:
            field_dict["resources"] = resources
        if restart is not UNSET:
            field_dict["restart"] = restart
        if secrets is not UNSET:
            field_dict["secrets"] = secrets
        if snapshot is not UNSET:
            field_dict["snapshot"] = snapshot
        if user is not UNSET:
            field_dict["user"] = user
        if workdir is not UNSET:
            field_dict["workdir"] = workdir

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.resource_request import ResourceRequest  # noqa: PLC0415
        from ..models.restart_spec import RestartSpec  # noqa: PLC0415

        d = dict(src_dict)
        command = cast(list[str], d.pop("command", UNSET))

        env = cast(list[str], d.pop("env", UNSET))

        image = d.pop("image", UNSET)

        name = d.pop("name", UNSET)

        policy = d.pop("policy", UNSET)

        _resources = d.pop("resources", UNSET)
        resources: ResourceRequest | Unset
        if isinstance(_resources, Unset):
            resources = UNSET
        else:
            resources = ResourceRequest.from_dict(_resources)

        _restart = d.pop("restart", UNSET)
        restart: RestartSpec | Unset
        if isinstance(_restart, Unset):
            restart = UNSET
        else:
            restart = RestartSpec.from_dict(_restart)

        secrets = cast(list[str], d.pop("secrets", UNSET))

        snapshot = d.pop("snapshot", UNSET)

        user = d.pop("user", UNSET)

        workdir = d.pop("workdir", UNSET)

        create_request = cls(
            command=command,
            env=env,
            image=image,
            name=name,
            policy=policy,
            resources=resources,
            restart=restart,
            secrets=secrets,
            snapshot=snapshot,
            user=user,
            workdir=workdir,
        )

        return create_request
