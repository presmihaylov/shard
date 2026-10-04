"""What both clients share on the wire: paths, JSON bodies, and how a dropped call is named."""

from __future__ import annotations

import json
import urllib.parse
from collections.abc import Mapping, Sequence
from typing import Any

import attrs
import httpx

from ._types import TerminalSize
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
    """The body of a command start. A string runs under /bin/sh -c, a sequence as the argv itself."""
    argv = ["/bin/sh", "-c", command] if isinstance(command, str) else list(command)
    if not argv:
        raise ValueError("command must name a program")
    body: dict[str, Any] = {"command": argv, "stdin": stdin, "tty": tty is not False, "attach": True}
    if env:
        body["env"] = [f"{key}={value}" for key, value in env.items()]
    if workdir:
        body["workdir"] = workdir
    if user:
        body["user"] = user
    if isinstance(tty, TerminalSize):
        body["size"] = {"rows": tty.rows, "cols": tty.cols}
    return body


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
