from __future__ import annotations

import asyncio
import json
import threading
import time
from collections.abc import Iterator
from typing import Any

import pytest
from fakedaemon import RECORD, Answer, FakeDaemon, Peer

from useshards import (
    AppExit,
    AsyncShard,
    Capabilities,
    ConflictError,
    HostAddress,
    NotFoundError,
    PermissionDeniedError,
    Policy,
    PolicyRule,
    Port,
    PortForward,
    ProtocolError,
    Restart,
    SandboxList,
    SecretList,
    ServerError,
    Shard,
    ShardConnectionError,
    UnsupportedError,
)
from useshards._wire import EXIT, STDOUT

SANDBOX: dict[str, Any] = {
    "id": "sb",
    "name": "web",
    "image": "docker.io/library/alpine:3",
    "provider": "gvisor",
    "state": "running",
    "resources": {"memory_mib": 512, "vcpus": 1, "disk_mib": 0},
    "created_at": "2026-10-04T10:00:00Z",
}
RULE = {
    "action": "allow",
    "destination": {"kind": "domain-suffix", "value": "example.com"},
    "protocol": "tcp",
    "ports": [443],
}
PORT_RECORD: dict[str, Any] = {
    "sandbox": "sb",
    "sandbox_name": "web",
    "host_port": 9000,
    "guest_port": 8000,
    "public": False,
    "address": "127.0.0.1",
    "listening": True,
    "reachable_on": [{"interface": "lo", "address": "127.0.0.1"}],
}
EGRESS_RECORD = {
    "time": "2026-10-04T10:00:01Z",
    "source": "proxy",
    "verdict": "allow",
    "host": "example.com",
    "port": 443,
    "rule": "r1",
}


@pytest.fixture
def daemon() -> Iterator[FakeDaemon]:
    fake = FakeDaemon()
    fake.routes[("GET", "/v0/sandboxes/sb")] = (200, SANDBOX)
    yield fake
    fake.close()
    assert fake.errors == []


@pytest.fixture
def shard(daemon: FakeDaemon) -> Iterator[Shard]:
    with pytest.warns(UserWarning, match="HTTP does not encrypt"):
        client = Shard(daemon.url, "k")
    with client:
        yield client


def async_shard(daemon: FakeDaemon) -> AsyncShard:
    with pytest.warns(UserWarning, match="HTTP does not encrypt"):
        return AsyncShard(daemon.url, "k")


def sent(daemon: FakeDaemon, method: str, path: str) -> Any:
    return next(json.loads(body) for m, p, body in daemon.requests if (m, p) == (method, path))


def log_end(peer: Peer) -> str:
    peer.send(EXIT, json.dumps({"reason": "stopped"}).encode())
    return peer.until_end()


def test_a_policy_list_leaves_out_holders_and_dns(daemon: FakeDaemon, shard: Shard) -> None:
    web = {"name": "web", "rules": [RULE]}
    daemon.routes[("GET", "/v0/policies/web")] = (200, {**web, "holders": ["sb"], "dns": "closed"})
    daemon.routes[("GET", "/v0/policies")] = (200, {"policies": [web], "next": "c2"})
    daemon.routes[("GET", "/v0/policies?cursor=c2")] = (200, {"policies": [{"name": "open", "rules": []}], "next": ""})
    rule = PolicyRule(action="allow", rule="suffix:example.com tcp:443")
    assert shard.policies.get("web") == Policy(name="web", rules=(rule,), holders=("sb",), dns="closed")
    assert [(each.name, each.holders, each.dns) for each in shard.policies.list()] == [
        ("web", None, None),
        ("open", None, None),
    ]
    assert daemon.targets[-2:] == ["/v0/policies", "/v0/policies?cursor=c2"]


@pytest.mark.parametrize("asynchronous", [False, True])
@pytest.mark.parametrize("kind", ["sandboxes", "secrets"])
def test_partial_lists_keep_each_warning_once_in_first_seen_order(
    daemon: FakeDaemon, shard: Shard, kind: str, asynchronous: bool
) -> None:
    route = f"/v0/{kind}"
    row = dict(SANDBOX)
    if kind == "secrets":
        row = {"name": "token", "destinations": [], "placeholder": "ph", "updated_at": "2026-10-04T10:00:00Z"}
    query = "all=true&" if kind == "sandboxes" else ""
    daemon.routes[("GET", route)] = (200, {kind: [], "next": "c2", "warnings": ["second", "first", "second"]})
    daemon.routes[("GET", f"{route}?{query}cursor=c2")] = (
        200,
        {kind: [row], "next": "c3", "warnings": ["first", "third", "First"]},
    )
    daemon.routes[("GET", f"{route}?{query}cursor=c3")] = (200, {kind: [], "next": None})

    async def listed() -> SandboxList[Any] | SecretList:
        async with async_shard(daemon) as client:
            if kind == "sandboxes":
                return await client.list(all=True)
            return await client.secrets.list()

    result: SandboxList[Any] | SecretList
    if asynchronous:
        result = asyncio.run(listed())
    if not asynchronous:
        if kind == "sandboxes":
            result = shard.list(all=True)
        if kind == "secrets":
            result = shard.secrets.list()
    assert result.warnings == ["second", "first", "third", "First"]
    if kind == "sandboxes":
        assert isinstance(result, SandboxList)
        assert [each.id for each in result.sandboxes] == ["sb"]
    if kind == "secrets":
        assert isinstance(result, SecretList)
        assert [each.name for each in result.secrets] == ["token"]


@pytest.mark.parametrize("asynchronous", [False, True])
@pytest.mark.parametrize("holders", [None, [], ["synthetic-sandbox"]])
def test_a_conflict_keeps_optional_holders(
    daemon: FakeDaemon, shard: Shard, asynchronous: bool, holders: list[str] | None
) -> None:
    error: dict[str, Any] = {"code": "in_use", "message": "the secret has a grant"}
    if holders is not None:
        error["holders"] = holders
    daemon.routes[("DELETE", "/v0/secrets/token")] = (409, {"error": error})

    async def remove() -> None:
        async with async_shard(daemon) as client:
            await client.secrets.remove("token")

    with pytest.raises(ConflictError) as raised:
        if asynchronous:
            asyncio.run(remove())
        if not asynchronous:
            shard.secrets.remove("token")
    assert raised.value.code == "in_use"
    assert raised.value.holders == holders


def test_a_page_without_next_is_refused(daemon: FakeDaemon, shard: Shard) -> None:
    daemon.routes[("GET", "/v0/snapshots")] = (200, {"snapshots": []})
    with pytest.raises(ProtocolError, match="cannot read: KeyError..next"):
        shard.snapshots.list()


def test_capabilities(daemon: FakeDaemon, shard: Shard) -> None:
    verbs = {
        "create": True,
        "start": True,
        "stop": True,
        "remove": True,
        "pause": False,
        "resume": False,
        "fork": False,
        "snapshot": True,
        "port": True,
    }
    daemon.routes[("GET", "/v0/capabilities")] = (200, verbs)
    assert shard.capabilities() == Capabilities(**verbs)


def test_create_and_run_bodies(daemon: FakeDaemon, shard: Shard) -> None:
    daemon.routes[("POST", "/v0/sandboxes")] = (201, SANDBOX)
    assert shard.create("alpine:3", name="web", env={"MODE": "test"}, secrets=["TOKEN"]).id == "sb"
    app = shard.run("alpine:3", "sleep 1", memory_mib=0, restart=Restart(policy="on-failure", retries=3, backoff=2))
    assert app.sandbox.id == "sb"
    assert [json.loads(body) for _, _, body in daemon.requests] == [
        {
            "image": "alpine:3",
            "name": "web",
            "env": ["MODE=test"],
            "secrets": ["TOKEN"],
            "resources": {"vcpus": 0, "disk_mib": 0},
        },
        {
            "image": "alpine:3",
            "command": ["/bin/sh", "-c", "sleep 1"],
            "resources": {"vcpus": 0, "disk_mib": 0, "memory_mib": 0},
            "restart": {"policy": "on-failure", "backoff": 2, "retries": 3},
        },
    ]
    assert daemon.targets == ["/v0/sandboxes?wait=true"] * 2
    with pytest.raises(ValueError, match="exactly one"):
        shard.create()
    assert len(daemon.requests) == 2


def test_a_create_forwards_its_ports_private_unless_it_says_public(daemon: FakeDaemon, shard: Shard) -> None:
    daemon.routes[("POST", "/v0/sandboxes")] = (201, {**SANDBOX, "ports": [{"host_port": 9000, "guest_port": 8000}]})
    sandbox = shard.create("alpine:3", ports=[PortForward(9000, 8000), PortForward(9443, 443, public=True)])
    assert sent(daemon, "POST", "/v0/sandboxes")["ports"] == [
        {"host_port": 9000, "guest_port": 8000},
        {"host_port": 9443, "guest_port": 443, "public": True},
    ]
    assert sandbox.info.ports == (PortForward(host_port=9000, guest_port=8000, public=False),)
    assert shard.get("sb").info.ports == ()


@pytest.mark.parametrize("asynchronous", [False, True])
def test_port_add_list_and_remove(daemon: FakeDaemon, shard: Shard, asynchronous: bool) -> None:
    daemon.routes[("PUT", "/v0/sandboxes/sb/ports/9000")] = (200, {**PORT_RECORD, "public": True, "address": "0.0.0.0"})
    daemon.routes[("GET", "/v0/sandboxes/sb/ports")] = (200, {"ports": [PORT_RECORD], "next": "9000"})
    daemon.routes[("GET", "/v0/sandboxes/sb/ports?cursor=9000")] = (
        200,
        {
            "ports": [{**PORT_RECORD, "host_port": 9001, "listening": False, "error": "refused", "reachable_on": []}],
            "next": None,
        },
    )
    daemon.routes[("GET", "/v0/ports")] = (200, {"ports": [{**PORT_RECORD, "sandbox_name": ""}], "next": None})
    daemon.routes[("DELETE", "/v0/sandboxes/sb/ports/9000")] = (204, None)

    async def drive() -> tuple[Port, list[Port], list[Port]]:
        async with async_shard(daemon) as client:
            sandbox = await client.get("sb")
            added = await sandbox.ports.add(9000, 8000, public=True)
            listed = await sandbox.ports.list()
            every = await client.ports()
            await sandbox.ports.remove(9000)
            return added, listed, every

    if asynchronous:
        added, listed, every = asyncio.run(drive())
    if not asynchronous:
        sandbox = shard.get("sb")
        added, listed, every = sandbox.ports.add(9000, 8000, public=True), sandbox.ports.list(), shard.ports()
        sandbox.ports.remove(9000)
    assert added == Port(
        sandbox="sb",
        sandbox_name="web",
        host_port=9000,
        guest_port=8000,
        public=True,
        address="0.0.0.0",
        listening=True,
        error=None,
        reachable_on=(HostAddress(interface="lo", address="127.0.0.1"),),
    )
    assert sent(daemon, "PUT", "/v0/sandboxes/sb/ports/9000") == {"guest_port": 8000, "public": True}
    assert [(each.host_port, each.listening, each.error) for each in listed] == [
        (9000, True, None),
        (9001, False, "refused"),
    ]
    assert [(each.sandbox, each.sandbox_name) for each in every] == [("sb", None)]
    assert ("DELETE", "/v0/sandboxes/sb/ports/9000") in [(m, p) for m, p, _ in daemon.requests]


def test_a_private_port_add_sends_no_public(daemon: FakeDaemon, shard: Shard) -> None:
    daemon.routes[("PUT", "/v0/sandboxes/sb/ports/9000")] = (200, PORT_RECORD)
    shard.get("sb").ports.add(9000, 8000)
    assert sent(daemon, "PUT", "/v0/sandboxes/sb/ports/9000") == {"guest_port": 8000}


@pytest.mark.parametrize(
    ("status", "code", "kind"),
    [
        (404, "not_found", NotFoundError),
        (409, "in_use", ConflictError),
        (409, "unsupported", UnsupportedError),
        (403, "forbidden", PermissionDeniedError),
    ],
)
def test_a_port_refusal_raises_the_error_of_its_code(
    daemon: FakeDaemon, shard: Shard, status: int, code: str, kind: type[Exception]
) -> None:
    refusal = {"error": {"code": code, "message": f"refused as {code}"}}
    daemon.routes[("PUT", "/v0/sandboxes/sb/ports/9000")] = (status, refusal)
    daemon.routes[("DELETE", "/v0/sandboxes/sb/ports/9000")] = (status, refusal)
    sandbox = shard.get("sb")
    with pytest.raises(kind, match=f"refused as {code}"):
        sandbox.ports.add(9000, 8000)
    with pytest.raises(kind, match=f"refused as {code}"):
        sandbox.ports.remove(9000)


def test_exec_refuses_stdin_of_the_other_mode(daemon: FakeDaemon, shard: Shard) -> None:
    sandbox = shard.get("sb")
    with pytest.raises(TypeError, match="stdin is a bool"):
        sandbox.exec("cat", background=True, stdin=b"x")  # type: ignore[call-overload]
    with pytest.raises(TypeError, match="only a background command"):
        sandbox.exec("cat", stdin=True)  # type: ignore[call-overload]
    assert [method for method, _, _ in daemon.requests] == ["GET"]


def test_a_fork_names_the_sandbox_it_was_forked_from(daemon: FakeDaemon, shard: Shard) -> None:
    daemon.routes[("GET", "/v0/sandboxes/sb2")] = (200, {**SANDBOX, "id": "sb2", "forked_from": "sb"})
    assert shard.get("sb2").info.forked_from == "sb"
    assert shard.get("sb").info.forked_from is None


def test_changes_keep_the_handle_current(daemon: FakeDaemon, shard: Shard) -> None:
    daemon.routes[("POST", "/v0/sandboxes/sb/secrets/TOKEN")] = (200, {**SANDBOX, "secrets": ["TOKEN"]})
    daemon.routes[("PUT", "/v0/sandboxes/sb/policy")] = (200, {**SANDBOX, "policy": "web"})
    daemon.routes[("POST", "/v0/sandboxes/sb/stop")] = (200, {**SANDBOX, "state": "stopped"})
    daemon.routes[("DELETE", "/v0/sandboxes/sb")] = (204, None)
    sandbox = shard.get("sb")
    assert repr(sandbox) == "Sandbox(id='sb', name='web', state='running')"
    shard.secrets.grant(sandbox, "TOKEN")
    assert sandbox.info.secrets == ("TOKEN",)
    shard.policies.attach(sandbox, "web")
    assert (sandbox.info.policy, sent(daemon, "PUT", "/v0/sandboxes/sb/policy")) == ("web", {"policy": "web"})
    sandbox.stop()
    assert sandbox.info.state == "stopped"
    sandbox.remove(force=True)
    assert daemon.targets[-1] == "/v0/sandboxes/sb?force=true"


def test_logs_and_egress_log(daemon: FakeDaemon, shard: Shard) -> None:
    daemon.routes[("GET", "/v0/sandboxes/sb/logs")] = (200, b"hello \xff")
    daemon.routes[("GET", "/v0/sandboxes/sb/egress-log")] = (200, [EGRESS_RECORD])
    sandbox = shard.get("sb")
    assert sandbox.logs() == "hello �"
    assert [(each.host, each.port, each.verdict) for each in sandbox.egress_log()] == [("example.com", 443, "allow")]


def test_commands_list_and_get(daemon: FakeDaemon, shard: Shard) -> None:
    daemon.routes[("GET", "/v0/sandboxes/sb/exec")] = (200, {"execs": [RECORD], "next": None})
    sandbox = shard.get("sb")
    assert [each.id for each in sandbox.commands.list()] == ["e1"]
    assert sandbox.commands.get("e1").id == "e1"


def test_follow_logs_to_the_end(daemon: FakeDaemon, shard: Shard) -> None:
    def session(peer: Peer) -> str:
        peer.send(STDOUT, b"a")
        peer.send(STDOUT, b"b")
        return log_end(peer)

    daemon.attaches = [session]
    with shard.get("sb").follow_logs() as follow:
        assert list(follow) == [b"a", b"b"]
    assert daemon.outcomes == ["close"]
    assert daemon.targets[-1] == "/v0/sandboxes/sb/logs?follow=true"


def test_follow_egress_log_to_a_normal_close(daemon: FakeDaemon, shard: Shard) -> None:
    def session(peer: Peer) -> str:
        peer.text(EGRESS_RECORD)
        return peer.close(1000)

    daemon.attaches = [session]
    assert [each.host for each in shard.get("sb").follow_egress_log()] == ["example.com"]
    assert daemon.targets[-1] == "/v0/sandboxes/sb/egress-log?follow=true"


def test_follow_raises_the_daemon_error(daemon: FakeDaemon, shard: Shard) -> None:
    daemon.attaches = [lambda peer: peer.close(1011, "the log tail failed")]
    with pytest.raises(ServerError, match="the log tail failed"):
        list(shard.get("sb").follow_logs())


def test_follow_raises_a_failure_message(daemon: FakeDaemon, shard: Shard) -> None:
    daemon.attaches = [lambda peer: peer.fail("not_found", "no sandbox sb")]
    with pytest.raises(NotFoundError, match="no sandbox sb"):
        list(shard.get("sb").follow_logs())


def test_follow_drop_is_a_connection_error(daemon: FakeDaemon, shard: Shard) -> None:
    daemon.attaches = [lambda peer: peer.send(STDOUT, b"a")]
    got: list[bytes] = []
    with pytest.raises(ShardConnectionError, match="dropped"):
        for chunk in shard.get("sb").follow_logs():
            got.append(chunk)
    assert got == [b"a"]


def test_close_from_another_thread_ends_a_blocked_read(daemon: FakeDaemon, shard: Shard) -> None:
    def session(peer: Peer) -> str:
        peer.send(STDOUT, b"a")
        return peer.until_end()

    daemon.attaches = [session]
    follow = shard.get("sb").follow_logs()
    got: list[bytes] = []
    errors: list[BaseException] = []

    def read() -> None:
        try:
            got.extend(follow)
        except BaseException as e:
            errors.append(e)

    reader = threading.Thread(target=read)
    reader.start()
    deadline = time.monotonic() + 5
    while not daemon.targets[-1].endswith("follow=true") and time.monotonic() < deadline:
        time.sleep(0.01)
    # Long enough for the reader to block on its second read.
    time.sleep(0.2)
    follow.close()
    reader.join(5)
    assert (reader.is_alive(), got, errors) == (False, [b"a"], [])
    daemon.close()
    assert daemon.outcomes == ["eof"]


def test_close_before_a_read_opens_nothing(daemon: FakeDaemon, shard: Shard) -> None:
    follow = shard.get("sb").follow_logs()
    follow.close()
    assert list(follow) == []
    assert daemon.targets == ["/v0/sandboxes/sb"]


def test_app_wait_timeout_leaves_the_app(daemon: FakeDaemon, shard: Shard) -> None:
    go = threading.Event()
    calls: list[int] = []

    def attach() -> Answer | None:
        calls.append(1)
        if len(calls) > 1:
            return (200, {"code": 0, "signal": 0, "restarts": 1})
        assert go.wait(5)
        return None

    daemon.routes[("POST", "/v0/sandboxes")] = (201, {**SANDBOX, "command": ["sleep", "60"]})
    daemon.routes[("GET", "/v0/sandboxes/sb/attach")] = attach
    app = shard.run("alpine:3", ["sleep", "60"])
    with pytest.raises(TimeoutError, match="did not end within 0.1s"):
        app.wait(timeout=0.1)
    go.set()
    assert app.wait() == AppExit(exit_code=0, signal=None, restarts=1)


def test_async_follow_to_the_end(daemon: FakeDaemon) -> None:
    def session(peer: Peer) -> str:
        peer.send(STDOUT, b"a")
        return log_end(peer)

    daemon.attaches = [session]

    async def run() -> list[bytes]:
        async with async_shard(daemon) as client:
            sandbox = await client.get("sb")
            async with sandbox.follow_logs() as follow:
                return [chunk async for chunk in follow]

    assert asyncio.run(run()) == [b"a"]
    assert daemon.outcomes == ["close"]


def test_async_cancel_lets_go_of_the_follow(daemon: FakeDaemon) -> None:
    def session(peer: Peer) -> str:
        peer.send(STDOUT, b"a")
        return peer.until_end()

    daemon.attaches = [session]

    async def run() -> list[bytes]:
        async with async_shard(daemon) as client:
            follow = (await client.get("sb")).follow_logs()
            first = asyncio.Event()

            async def consume() -> None:
                async with follow:
                    async for _ in follow:
                        first.set()

            task = asyncio.create_task(consume())
            await asyncio.wait_for(first.wait(), 5)
            await asyncio.sleep(0.05)
            task.cancel()
            with pytest.raises(asyncio.CancelledError):
                await task
            return [chunk async for chunk in follow]

    assert asyncio.run(run()) == []
    daemon.close()
    assert daemon.outcomes == ["eof"]
