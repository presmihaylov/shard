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
    from ..models.oom import OOM
    from ..models.port_forward import PortForward
    from ..models.process import Process
    from ..models.resources import Resources


T = TypeVar("T", bound="Inspection")


@_attrs_define
class Inspection:
    created_at: datetime.datetime
    id: str
    image: str
    provider: str
    resources: Resources
    state: InspectionState
    digest: str | Unset = UNSET
    egress: Effective | Unset = UNSET
    failed_reason: str | Unset = UNSET
    forked_from: str | Unset = UNSET
    kernel: str | Unset = UNSET
    name: str | Unset = UNSET
    oom: OOM | Unset = UNSET
    policy: str | Unset = UNSET
    ports: list[PortForward] | Unset = UNSET
    processes: list[Process] | Unset = UNSET
    secrets: list[str] | Unset = UNSET
    snapshot: str | Unset = UNSET
    started_at: datetime.datetime | Unset = UNSET
    stopped_reason: str | Unset = UNSET

    def to_dict(self) -> dict[str, Any]:
        from ..models.effective import Effective  # noqa: PLC0415
        from ..models.oom import OOM  # noqa: PLC0415
        from ..models.port_forward import PortForward  # noqa: PLC0415
        from ..models.process import Process  # noqa: PLC0415
        from ..models.resources import Resources  # noqa: PLC0415

        created_at = self.created_at.isoformat()

        id = self.id

        image = self.image

        provider = self.provider

        resources = self.resources.to_dict()

        state = self.state.value

        digest = self.digest

        egress: dict[str, Any] | Unset = UNSET
        if not isinstance(self.egress, Unset):
            egress = self.egress.to_dict()

        failed_reason = self.failed_reason

        forked_from = self.forked_from

        kernel = self.kernel

        name = self.name

        oom: dict[str, Any] | Unset = UNSET
        if not isinstance(self.oom, Unset):
            oom = self.oom.to_dict()

        policy = self.policy

        ports: list[dict[str, Any]] | Unset = UNSET
        if not isinstance(self.ports, Unset):
            ports = []
            for ports_item_data in self.ports:
                ports_item = ports_item_data.to_dict()
                ports.append(ports_item)

        processes: list[dict[str, Any]] | Unset = UNSET
        if not isinstance(self.processes, Unset):
            processes = []
            for processes_item_data in self.processes:
                processes_item = processes_item_data.to_dict()
                processes.append(processes_item)

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
        if digest is not UNSET:
            field_dict["digest"] = digest
        if egress is not UNSET:
            field_dict["egress"] = egress
        if failed_reason is not UNSET:
            field_dict["failed_reason"] = failed_reason
        if forked_from is not UNSET:
            field_dict["forked_from"] = forked_from
        if kernel is not UNSET:
            field_dict["kernel"] = kernel
        if name is not UNSET:
            field_dict["name"] = name
        if oom is not UNSET:
            field_dict["oom"] = oom
        if policy is not UNSET:
            field_dict["policy"] = policy
        if ports is not UNSET:
            field_dict["ports"] = ports
        if processes is not UNSET:
            field_dict["processes"] = processes
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
        from ..models.oom import OOM  # noqa: PLC0415
        from ..models.port_forward import PortForward  # noqa: PLC0415
        from ..models.process import Process  # noqa: PLC0415
        from ..models.resources import Resources  # noqa: PLC0415

        d = dict(src_dict)
        created_at = datetime.datetime.fromisoformat(d.pop("created_at"))

        id = d.pop("id")

        image = d.pop("image")

        provider = d.pop("provider")

        resources = Resources.from_dict(d.pop("resources"))

        state = InspectionState(d.pop("state"))

        digest = d.pop("digest", UNSET)

        _egress = d.pop("egress", UNSET)
        egress: Effective | Unset
        if isinstance(_egress, Unset):
            egress = UNSET
        else:
            egress = Effective.from_dict(_egress)

        failed_reason = d.pop("failed_reason", UNSET)

        forked_from = d.pop("forked_from", UNSET)

        kernel = d.pop("kernel", UNSET)

        name = d.pop("name", UNSET)

        _oom = d.pop("oom", UNSET)
        oom: OOM | Unset
        if isinstance(_oom, Unset):
            oom = UNSET
        else:
            oom = OOM.from_dict(_oom)

        policy = d.pop("policy", UNSET)

        _ports = d.pop("ports", UNSET)
        ports: list[PortForward] | Unset = UNSET
        if _ports is not UNSET:
            ports = []
            for ports_item_data in _ports:
                ports_item = PortForward.from_dict(ports_item_data)

                ports.append(ports_item)

        _processes = d.pop("processes", UNSET)
        processes: list[Process] | Unset = UNSET
        if _processes is not UNSET:
            processes = []
            for processes_item_data in _processes:
                processes_item = Process.from_dict(processes_item_data)

                processes.append(processes_item)

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
            digest=digest,
            egress=egress,
            failed_reason=failed_reason,
            forked_from=forked_from,
            kernel=kernel,
            name=name,
            oom=oom,
            policy=policy,
            ports=ports,
            processes=processes,
            secrets=secrets,
            snapshot=snapshot,
            started_at=started_at,
            stopped_reason=stopped_reason,
        )

        return inspection
