"""The connection settings: an explicit option beats the environment, as the shard CLI reads them."""

from __future__ import annotations

import os
import ssl
import urllib.parse
from collections.abc import Mapping

import attrs

from .errors import ConfigurationError

REMOTE_ENV = "SHARD_REMOTE"
API_KEY_ENV = "SHARD_API_KEY"
CA_FILE_ENV = "SHARD_CA_FILE"
# The CLI's line for an http remote, less the "Warning: " that Python's UserWarning label already prints.
PLAIN_WARNING = (
    "HTTP does not encrypt this connection. Use it only on localhost or through a trusted encrypted network."
)
_PORTS = {"http": 80, "https": 443}

StrPath = str | os.PathLike[str]


@attrs.frozen(repr=False)
class Settings:
    base_url: str
    api_key: str
    verify: ssl.SSLContext | bool

    def __repr__(self) -> str:
        return f"Settings(base_url={self.base_url!r})"

    @property
    def plain(self) -> bool:
        """An http remote, whose bytes, the key among them, cross the network in the clear."""
        return self.base_url.startswith("http://")


def resolve(
    remote: str | None,
    api_key: str | None,
    ca_file: StrPath | None,
    env: Mapping[str, str] | None = None,
) -> Settings:
    environ = os.environ if env is None else env
    base_url = _base_url(remote, environ)
    return Settings(
        base_url=base_url,
        api_key=_api_key(api_key, environ),
        verify=_verify(ca_file, environ, base_url),
    )


def _base_url(remote: str | None, env: Mapping[str, str]) -> str:
    source = "remote"
    value = remote
    if not value:
        source, value = REMOTE_ENV, env.get(REMOTE_ENV, "")
    if not value:
        raise ConfigurationError(f"no remote: pass remote= or set {REMOTE_ENV}, as https://shard.example.com")
    parsed = urllib.parse.urlsplit(value)
    if parsed.scheme not in _PORTS or not parsed.hostname:
        raise ConfigurationError(f"{source} must be an http or https url with a host, as https://shard.example.com")
    if parsed.username is not None or parsed.query or parsed.fragment or parsed.path not in ("", "/"):
        raise ConfigurationError(f"{source} must name only a scheme, a host and a port, as https://shard.example.com")
    try:
        port = parsed.port or _PORTS[parsed.scheme]
    except ValueError:
        raise ConfigurationError(f"{source} names a port that is not a number from 0 to 65535") from None
    host = f"[{parsed.hostname}]" if ":" in parsed.hostname else parsed.hostname
    return f"{parsed.scheme}://{host}:{port}"


def _api_key(api_key: str | None, env: Mapping[str, str]) -> str:
    if api_key is not None:
        return _checked(api_key.strip(), "api_key")
    key = env.get(API_KEY_ENV, "").strip()
    if key:
        return _checked(key, API_KEY_ENV)
    raise ConfigurationError(f"no API key: pass api_key= or set {API_KEY_ENV}; shard serve answers 401 without one")


def _checked(token: str, source: str) -> str:
    if not token:
        raise ConfigurationError(f"{source} is empty")
    if any((c < " " and c != "\t") or c == "\x7f" for c in token):
        raise ConfigurationError(f"{source} holds a control character, which no HTTP header carries")
    return token


def _verify(ca_file: StrPath | None, env: Mapping[str, str], base_url: str) -> ssl.SSLContext | bool:
    source = "ca_file"
    path = os.fspath(ca_file) if ca_file is not None else ""
    if not path:
        source, path = CA_FILE_ENV, env.get(CA_FILE_ENV, "")
    if not path:
        return True
    if base_url.startswith("http://"):
        raise ConfigurationError(
            f"{source} is set, and the remote {base_url} is http: a CA certificate verifies an https remote only"
        )
    try:
        return ssl.create_default_context(cafile=path)
    except OSError as e:
        raise ConfigurationError(f"{source}: read the CA file {path}: {e.strerror}") from None
    except ssl.SSLError:
        raise ConfigurationError(f"{source}: the CA file {path} holds no certificate") from None
