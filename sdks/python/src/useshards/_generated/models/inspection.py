from __future__ import annotations

import datetime
from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, BinaryIO, Generator, TextIO, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.inspection_state import InspectionState
from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.effective import Effective
    from ..models.exit_status import ExitStatus
    from ..models.resources import Resources
    from ..models.restart import Restart


T = TypeVar("T", bound="Inspection")


@_attrs_define
class Inspection:
    created_at: datetime.datetime
    id: str
    image: str
    provider: str
    resources: Resources
    state: InspectionState
    command: list[str] | Unset = UNSET
    digest: str | Unset = UNSET
    egress: Effective | Unset = UNSET
    exit_status: ExitStatus | Unset = UNSET
    failed_reason: str | Unset = UNSET
    forked_from: str | Unset = UNSET
    kernel: str | Unset = UNSET
    name: str | Unset = UNSET
    policy: str | Unset = UNSET
    restart: Restart | Unset = UNSET
    secrets: list[str] | Unset = UNSET
    snapshot: str | Unset = UNSET
    started_at: datetime.datetime | Unset = UNSET
    stopped_reason: str | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        from ..models.effective import Effective  # noqa: PLC0415
        from ..models.exit_status import ExitStatus  # noqa: PLC0415
        from ..models.resources import Resources  # noqa: PLC0415
        from ..models.restart import Restart  # noqa: PLC0415

        created_at = self.created_at.isoformat()

        id = self.id

        image = self.image

        provider = self.provider

        resources = self.resources.to_dict()

        state = self.state.value

        command: list[str] | Unset = UNSET
        if not isinstance(self.command, Unset):
            command = self.command

        digest = self.digest

        egress: dict[str, Any] | Unset = UNSET
        if not isinstance(self.egress, Unset):
            egress = self.egress.to_dict()

        exit_status: dict[str, Any] | Unset = UNSET
        if not isinstance(self.exit_status, Unset):
            exit_status = self.exit_status.to_dict()

        failed_reason = self.failed_reason

        forked_from = self.forked_from

        kernel = self.kernel

        name = self.name

        policy = self.policy

        restart: dict[str, Any] | Unset = UNSET
        if not isinstance(self.restart, Unset):
            restart = self.restart.to_dict()

        secrets: list[str] | Unset = UNSET
        if not isinstance(self.secrets, Unset):
            secrets = self.secrets

        snapshot = self.snapshot

        started_at: str | Unset = UNSET
        if not isinstance(self.started_at, Unset):
            started_at = self.started_at.isoformat()

        stopped_reason = self.stopped_reason

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "created_at": created_at,
                "id": id,
                "image": image,
                "provider": provider,
                "resources": resources,
                "state": state,
            }
        )
        if command is not UNSET:
            field_dict["command"] = command
        if digest is not UNSET:
            field_dict["digest"] = digest
        if egress is not UNSET:
            field_dict["egress"] = egress
        if exit_status is not UNSET:
            field_dict["exit_status"] = exit_status
        if failed_reason is not UNSET:
            field_dict["failed_reason"] = failed_reason
        if forked_from is not UNSET:
            field_dict["forked_from"] = forked_from
        if kernel is not UNSET:
            field_dict["kernel"] = kernel
        if name is not UNSET:
            field_dict["name"] = name
        if policy is not UNSET:
            field_dict["policy"] = policy
        if restart is not UNSET:
            field_dict["restart"] = restart
        if secrets is not UNSET:
            field_dict["secrets"] = secrets
        if snapshot is not UNSET:
            field_dict["snapshot"] = snapshot
        if started_at is not UNSET:
            field_dict["started_at"] = started_at
        if stopped_reason is not UNSET:
            field_dict["stopped_reason"] = stopped_reason

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.effective import Effective  # noqa: PLC0415
        from ..models.exit_status import ExitStatus  # noqa: PLC0415
        from ..models.resources import Resources  # noqa: PLC0415
        from ..models.restart import Restart  # noqa: PLC0415

        d = dict(src_dict)
        created_at = datetime.datetime.fromisoformat(d.pop("created_at"))

        id = d.pop("id")

        image = d.pop("image")

        provider = d.pop("provider")

        resources = Resources.from_dict(d.pop("resources"))

        state = InspectionState(d.pop("state"))

        command = cast(list[str], d.pop("command", UNSET))

        digest = d.pop("digest", UNSET)

        _egress = d.pop("egress", UNSET)
        egress: Effective | Unset
        if isinstance(_egress, Unset):
            egress = UNSET
        else:
            egress = Effective.from_dict(_egress)

        _exit_status = d.pop("exit_status", UNSET)
        exit_status: ExitStatus | Unset
        if isinstance(_exit_status, Unset):
            exit_status = UNSET
        else:
            exit_status = ExitStatus.from_dict(_exit_status)

        failed_reason = d.pop("failed_reason", UNSET)

        forked_from = d.pop("forked_from", UNSET)

        kernel = d.pop("kernel", UNSET)

        name = d.pop("name", UNSET)

        policy = d.pop("policy", UNSET)

        _restart = d.pop("restart", UNSET)
        restart: Restart | Unset
        if isinstance(_restart, Unset):
            restart = UNSET
        else:
            restart = Restart.from_dict(_restart)

        secrets = cast(list[str], d.pop("secrets", UNSET))

        snapshot = d.pop("snapshot", UNSET)

        _started_at = d.pop("started_at", UNSET)
        started_at: datetime.datetime | Unset
        if isinstance(_started_at, Unset):
            started_at = UNSET
        else:
            started_at = datetime.datetime.fromisoformat(_started_at)

        stopped_reason = d.pop("stopped_reason", UNSET)

        inspection = cls(
            created_at=created_at,
            id=id,
            image=image,
            provider=provider,
            resources=resources,
            state=state,
            command=command,
            digest=digest,
            egress=egress,
            exit_status=exit_status,
            failed_reason=failed_reason,
            forked_from=forked_from,
            kernel=kernel,
            name=name,
            policy=policy,
            restart=restart,
            secrets=secrets,
            snapshot=snapshot,
            started_at=started_at,
            stopped_reason=stopped_reason,
        )

        return inspection
