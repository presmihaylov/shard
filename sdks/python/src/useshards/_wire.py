"""What both clients share on the wire: paths, JSON bodies, and how a dropped call is named."""

from __future__ import annotations

import json
import urllib.parse
from collections.abc import Awaitable, Callable, Mapping, Sequence
from typing import Any

import attrs
import httpx

from ._generated import models
from ._generated.types import UNSET, Response, Unset
from ._types import PortForward, Restart, TerminalSize
from .errors import APIError, CommandNotStartedError, ProtocolError, ShardConnectionError, failure_error

# A generated call bound to its arguments, which a transport runs; a fetch is one page of a list by its cursor.
AsyncCall = Callable[[], Awaitable[Response[Any]]]
Call = Callable[[], Response[Any]]
AsyncFetch = Callable[[str | Unset], Awaitable[Response[Any]]]
Fetch = Callable[[str | Unset], Response[Any]]

# The stream a command message carries in its first byte: the client sends stdin and stdin closed, the daemon the rest.
STDIN = 0
STDOUT = 1
STDERR = 2
EXIT = 3
STDIN_CLOSE = 4
FAILURE = 5


def path(*parts: str) -> str:
    return "/v0/" + "/".join(urllib.parse.quote(part, safe="") for part in parts)


def connection_error(request: httpx.Request, e: httpx.TransportError) -> ShardConnectionError:
    if isinstance(e, httpx.TimeoutException):
        return ShardConnectionError(f"{request.method} {request.url.path}: the daemon did not answer in time")
    return ShardConnectionError(f"{request.method} {request.url.path}: {e or type(e).__name__}")


def exec_body(
    command: str | Sequence[str],
    *,
    env: Mapping[str, str] | None,
    workdir: str | None,
    user: str | None,
    stdin: bool,
    tty: bool | TerminalSize,
) -> models.ExecRequest:
    """The body of a command start."""
    return models.ExecRequest(
        command=argv(command),
        stdin=stdin,
        tty=tty is not False,
        attach=True,
        env=_env(env),
        workdir=workdir or UNSET,
        user=user or UNSET,
        size=models.TerminalSize(rows=tty.rows, cols=tty.cols) if isinstance(tty, TerminalSize) else UNSET,
    )


def create_body(
    image: str | None,
    *,
    snapshot: str | None,
    name: str | None,
    env: Mapping[str, str] | None,
    workdir: str | None,
    user: str | None,
    secrets: Sequence[str] | None,
    policy: str | None,
    memory_mib: int | None,
    vcpus: int | None,
    disk_mib: int | None,
    ports: Sequence[PortForward] | None,
    swap_mib: int | None,
) -> models.CreateRequest:
    """The body of a create. A None memory takes the snapshot's or provider's bound, any other None the default."""
    if (image is None) == (snapshot is None):
        raise ValueError("a sandbox is made from an image or a snapshot, exactly one of them")
    return models.CreateRequest(
        resources=models.ResourceRequest(
            vcpus=vcpus or 0,
            disk_mib=disk_mib or 0,
            memory_mib=UNSET if memory_mib is None else memory_mib,
            swap_mib=UNSET if swap_mib is None else swap_mib,
        ),
        env=_env(env),
        workdir=workdir or UNSET,
        user=user or UNSET,
        image=UNSET if image is None else image,
        snapshot=UNSET if snapshot is None else snapshot,
        name=name or UNSET,
        secrets=list(secrets) if secrets else UNSET,
        policy=policy or UNSET,
        ports=[_port_forward(each) for each in ports] if ports else UNSET,
    )


def run_body(
    command: str | Sequence[str],
    *,
    name: str | None,
    env: Mapping[str, str] | None,
    workdir: str | None,
    user: str | None,
    restart: Restart | None,
) -> models.RunRequest:
    """The body of a run. None env, workdir and user take the sandbox's own, and a None restart is unless-stopped."""
    return models.RunRequest(
        command=argv(command),
        name=name or UNSET,
        env=_env(env),
        workdir=workdir or UNSET,
        user=user or UNSET,
        restart=UNSET if restart is None else _restart_spec(restart),
    )


def argv(command: str | Sequence[str]) -> list[str]:
    """A string runs under /bin/sh -c, a sequence as the argv itself."""
    args = ["/bin/sh", "-c", command] if isinstance(command, str) else list(command)
    if not args:
        raise ValueError("command must name a program")
    return args


def _env(env: Mapping[str, str] | None) -> list[str] | Unset:
    return [f"{key}={value}" for key, value in env.items()] if env else UNSET


def _port_forward(forward: PortForward) -> models.PortForward:
    return models.PortForward(
        host_port=forward.host_port, guest_port=forward.guest_port, public=forward.public or UNSET
    )


def _restart_spec(restart: Restart) -> models.RestartSpec:
    return models.RestartSpec(
        policy=models.RestartSpecPolicy(restart.policy),
        backoff=restart.backoff or UNSET,
        retries=restart.retries or UNSET,
    )


@attrs.frozen
class Exit:
    code: int
    signal: int | None
    lost_bytes: int


def exit_of(payload: bytes, what: str) -> Exit:
    """Read an exit message; one that names an error is a command the sandbox never ran."""
    message = _object(payload, f"the exit of {what}")
    code = message.get("code")
    signal = message.get("signal", 0)
    lost = message.get("lost_bytes", 0)
    error = message.get("error", "")
    if not isinstance(code, int) or not isinstance(signal, int) or not isinstance(lost, int):
        raise ProtocolError(f"the daemon answered {payload!r} as the exit of {what}")
    if not isinstance(error, str):
        raise ProtocolError(f"the daemon answered {payload!r} as the exit of {what}")
    if error:
        raise CommandNotStartedError(code, error)
    return Exit(code=code, signal=signal or None, lost_bytes=lost)


def failure_of(payload: bytes, what: str) -> APIError:
    """Read a failure message into the error a refusal before the handshake would have been."""
    err = _object(payload, f"the failure of {what}").get("error")
    code = err.get("code") if isinstance(err, dict) else None
    message = err.get("message") if isinstance(err, dict) else None
    if not isinstance(code, str) or not isinstance(message, str) or not message:
        raise ProtocolError(f"the daemon answered {payload!r} as the failure of {what}")
    return failure_error(code, message)


def _object(payload: bytes, what: str) -> dict[str, Any]:
    try:
        decoded = json.loads(payload)
    except ValueError:
        raise ProtocolError(f"the daemon answered {payload!r} as {what}") from None
    if not isinstance(decoded, dict):
        raise ProtocolError(f"the daemon answered {payload!r} as {what}")
    return decoded
