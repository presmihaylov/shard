"""What both clients share on the wire: paths, JSON bodies, and how a dropped call is named."""

from __future__ import annotations

import json
import urllib.parse
from collections.abc import Mapping, Sequence
from typing import Any

import attrs
import httpx

from ._types import Restart, TerminalSize
from .errors import APIError, CommandNotStartedError, ProtocolError, ShardConnectionError, failure_error

# The stream a command message carries in its first byte: the client sends stdin and stdin closed, the daemon the rest.
STDIN = 0
STDOUT = 1
STDERR = 2
EXIT = 3
STDIN_CLOSE = 4
FAILURE = 5


def path(*parts: str) -> str:
    return "/v0/" + "/".join(urllib.parse.quote(part, safe="") for part in parts)


def json_of(response: httpx.Response) -> Any:
    try:
        return json.loads(response.content)
    except ValueError:
        raise ProtocolError(
            f"{response.request.method} {response.request.url.path} answered a body that is not JSON"
        ) from None


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
) -> dict[str, Any]:
    """The body of a command start."""
    body: dict[str, Any] = {"command": argv(command), "stdin": stdin, "tty": tty is not False, "attach": True}
    body.update(_process(env, workdir, user))
    if isinstance(tty, TerminalSize):
        body["size"] = {"rows": tty.rows, "cols": tty.cols}
    return body


def create_body(
    image: str | None,
    command: str | Sequence[str] | None,
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
    restart: Restart | None,
) -> dict[str, Any]:
    """The body of a create. A None memory takes the snapshot's bound or none, a None vcpus or disk the default."""
    if (image is None) == (snapshot is None):
        raise ValueError("a sandbox is made from an image or a snapshot, exactly one of them")
    resources: dict[str, Any] = {"vcpus": vcpus or 0, "disk_mib": disk_mib or 0}
    if memory_mib is not None:
        resources["memory_mib"] = memory_mib
    body: dict[str, Any] = {"resources": resources, **_process(env, workdir, user)}
    if image is not None:
        body["image"] = image
    if snapshot is not None:
        body["snapshot"] = snapshot
    if name:
        body["name"] = name
    if command is not None:
        body["command"] = argv(command)
    if secrets:
        body["secrets"] = list(secrets)
    if policy:
        body["policy"] = policy
    if restart is not None:
        body["restart"] = {"policy": restart.policy, "backoff": restart.backoff}
        if restart.retries:
            body["restart"]["retries"] = restart.retries
    return body


def argv(command: str | Sequence[str]) -> list[str]:
    """A string runs under /bin/sh -c, a sequence as the argv itself."""
    args = ["/bin/sh", "-c", command] if isinstance(command, str) else list(command)
    if not args:
        raise ValueError("command must name a program")
    return args


def _process(env: Mapping[str, str] | None, workdir: str | None, user: str | None) -> dict[str, Any]:
    fields: dict[str, Any] = {}
    if env:
        fields["env"] = [f"{key}={value}" for key, value in env.items()]
    if workdir:
        fields["workdir"] = workdir
    if user:
        fields["user"] = user
    return fields


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
