"""The exceptions the SDK raises. A nonzero exit is a result, never one of these."""

from __future__ import annotations

import json
from collections.abc import Mapping

__all__ = [
    "APIError",
    "AuthenticationError",
    "CommandNotStartedError",
    "ConfigurationError",
    "ConflictError",
    "InvalidRequestError",
    "NotFoundError",
    "PermissionDeniedError",
    "ProtocolError",
    "ServerError",
    "ShardConnectionError",
    "ShardError",
    "UnknownLengthError",
    "UnsafeArchiveError",
    "UnsupportedError",
]

# A body that is not the daemon's JSON (a proxy page, say) is quoted only this far.
_RAW_BODY_LIMIT = 512


class ShardError(Exception):
    """The base of every exception the SDK raises."""


class ConfigurationError(ShardError):
    """A setting is missing or refused. The message names the setting, never its value."""


class ShardConnectionError(ShardError):
    """The daemon could not be reached, or a stream ended before it said how the command ended."""


class ProtocolError(ShardError):
    """The daemon answered something the SDK cannot read."""


class UnknownLengthError(ShardError):
    """An upload's size is unknown, and the daemon refuses a body without a Content-Length."""


class UnsafeArchiveError(ShardError):
    """A sandbox's tar holds an entry a download refuses to land, as one that leaves the destination."""

    def __init__(self, entry: str, reason: str) -> None:
        super().__init__(f"refuse the entry {entry!r}: {reason}")
        self.entry = entry
        self.reason = reason


class CommandNotStartedError(ShardError):
    """The command never ran, as when its binary does not exist."""

    def __init__(self, exit_code: int, reason: str) -> None:
        super().__init__(f"the command did not start: {reason}")
        self.exit_code = exit_code
        self.reason = reason


class APIError(ShardError):
    """The daemon refused a request. code is the daemon's error code, empty when the body held none."""

    def __init__(self, status: int, code: str, message: str) -> None:
        super().__init__(f"{status} {code}: {message}" if code else f"{status}: {message}")
        self.status = status
        self.code = code
        self.message = message


class AuthenticationError(APIError):
    """401: the API key is missing, unknown or expired."""


class PermissionDeniedError(APIError):
    """403: the key's scopes do not cover the route, or the route is local to the host."""


class NotFoundError(APIError):
    """404: no such sandbox, snapshot, command, file, secret or policy."""


class InvalidRequestError(APIError):
    """400 or 413: the daemon refused the request as written."""


class ConflictError(APIError):
    """409: the request does not fit the current state, as a snapshot of a running sandbox."""


class UnsupportedError(APIError):
    """The provider has no such verb. The message names the provider and the verb."""


class ServerError(APIError):
    """5xx: the daemon or the provider failed."""


_CLASSES: Mapping[int, type[APIError]] = {
    400: InvalidRequestError,
    401: AuthenticationError,
    403: PermissionDeniedError,
    404: NotFoundError,
    409: ConflictError,
    413: InvalidRequestError,
}

# A stream failure carries a code and no status, so the status is the one the daemon answers that code with.
_CODE_STATUS: Mapping[str, int] = {
    "invalid_request": 400,
    "body_too_large": 413,
    "unauthorized": 401,
    "forbidden": 403,
    "not_found": 404,
    "sandbox_not_running": 409,
    "sandbox_not_stopped": 409,
    "sandbox_not_paused": 409,
    "sandbox_live": 409,
    "sandbox_failed": 409,
    "no_checkpoint": 409,
    "unsupported": 409,
    "in_use": 409,
    "name_taken": 409,
    "exec_exited": 409,
    "exec_running": 409,
    "no_app": 409,
    "app_ended": 409,
    "command_not_started": 422,
    "timeout": 504,
    "internal": 500,
}


def api_error(status: int, body: bytes) -> APIError | CommandNotStartedError:
    """Classify a refusal by the daemon's code, then by its status."""
    code, message, exit_code = _parse_body(body)
    if code == "command_not_started" and exit_code is not None:
        return CommandNotStartedError(exit_code, message)
    return _classified(status, code, message)


def failure_error(code: str, message: str) -> APIError:
    """Classify a failure the daemon sent on a stream after the handshake."""
    return _classified(_CODE_STATUS.get(code, 500), code, message)


def _classified(status: int, code: str, message: str) -> APIError:
    if code == "unsupported":
        return UnsupportedError(status, code, message)
    cls = _CLASSES.get(status, ServerError if status >= 500 else APIError)
    return cls(status, code, message)


def _parse_body(body: bytes) -> tuple[str, str, int | None]:
    try:
        decoded = json.loads(body)
    except (ValueError, UnicodeDecodeError):
        return "", _raw(body), None
    if not isinstance(decoded, dict):
        return "", _raw(body), None
    err = decoded.get("error")
    if not isinstance(err, dict):
        return "", _raw(body), None
    code = err.get("code")
    message = err.get("message")
    exit_code = err.get("exit_code")
    return (
        code if isinstance(code, str) else "",
        message if isinstance(message, str) else "",
        exit_code if isinstance(exit_code, int) and not isinstance(exit_code, bool) else None,
    )


def _raw(body: bytes) -> str:
    text = body[:_RAW_BODY_LIMIT].decode("utf-8", "replace").strip()
    return text or "an empty body"
