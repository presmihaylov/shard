"""The connection settings: an explicit option beats the environment, as the shard CLI reads them."""

from __future__ import annotations

import json
import os
import ssl
import stat
import urllib.parse
from collections.abc import Mapping

import attrs

from .errors import ConfigurationError

REMOTE_ENV = "SHARD_REMOTE"
API_KEY_ENV = "SHARD_API_KEY"
TOKEN_FILE_ENV = "SHARD_TOKEN_FILE"
CA_FILE_ENV = "SHARD_CA_FILE"

StrPath = str | os.PathLike[str]


@attrs.frozen(repr=False)
class Settings:
    base_url: str
    api_key: str
    verify: ssl.SSLContext | bool

    def __repr__(self) -> str:
        return f"Settings(base_url={self.base_url!r})"


def resolve(
    remote: str | None,
    api_key: str | None,
    token_file: StrPath | None,
    ca_file: StrPath | None,
    env: Mapping[str, str] | None = None,
) -> Settings:
    environ = os.environ if env is None else env
    return Settings(
        base_url=_base_url(remote, environ),
        api_key=_api_key(api_key, token_file, environ),
        verify=_verify(ca_file, environ),
    )


def _base_url(remote: str | None, env: Mapping[str, str]) -> str:
    source = "remote"
    value = remote
    if not value:
        source, value = REMOTE_ENV, env.get(REMOTE_ENV, "")
    if not value:
        raise ConfigurationError(f"no remote: pass remote= or set {REMOTE_ENV}, as https://shard.example.com")
    parsed = urllib.parse.urlsplit(value)
    if parsed.scheme != "https" or not parsed.hostname:
        raise ConfigurationError(f"{source} must be an https url with a host, as https://shard.example.com")
    if parsed.username is not None or parsed.query or parsed.fragment or parsed.path not in ("", "/"):
        raise ConfigurationError(f"{source} must name only a scheme, a host and a port, as https://shard.example.com")
    try:
        port = parsed.port or 443
    except ValueError:
        raise ConfigurationError(f"{source} names a port that is not a number from 0 to 65535") from None
    host = f"[{parsed.hostname}]" if ":" in parsed.hostname else parsed.hostname
    return f"https://{host}:{port}"


def _api_key(api_key: str | None, token_file: StrPath | None, env: Mapping[str, str]) -> str:
    if api_key is not None:
        return _checked(api_key.strip(), "api_key")
    if token_file is not None:
        return _read_token_file(os.fspath(token_file), "token_file")
    # A blank key is unset, so a stray export never shadows SHARD_TOKEN_FILE.
    key = env.get(API_KEY_ENV, "").strip()
    if key:
        return _checked(key, API_KEY_ENV)
    path = env.get(TOKEN_FILE_ENV, "")
    if path:
        return _read_token_file(path, TOKEN_FILE_ENV)
    raise ConfigurationError(
        f"no API key: pass api_key= or token_file=, or set {API_KEY_ENV} or {TOKEN_FILE_ENV}, in that order"
    )


def _read_token_file(path: str, source: str) -> str:
    try:
        mode = stat.S_IMODE(os.stat(path).st_mode)
        if mode & 0o007:
            raise ConfigurationError(f"{source}: the token file {path} is at mode {mode:04o}, which others can read")
        with open(path, encoding="utf-8") as f:
            raw = f.read().strip()
    except OSError as e:
        raise ConfigurationError(f"{source}: read the token file {path}: {e.strerror}") from None
    except UnicodeDecodeError:
        raise ConfigurationError(f"{source}: the token file {path} is not UTF-8 text") from None
    if not raw:
        raise ConfigurationError(f"{source}: the token file {path} holds no token")
    if not raw.startswith("{"):
        return _checked(raw, source)
    # tokens mint writes a JSON record; a token file holds it whole or the bare token.
    try:
        record = json.loads(raw)
    except ValueError:
        raise ConfigurationError(f"{source}: the token file {path} holds a record that is not JSON") from None
    token = record.get("token") if isinstance(record, dict) else None
    if not isinstance(token, str) or not token.strip():
        raise ConfigurationError(f"{source}: the record in the token file {path} holds no token")
    return _checked(token.strip(), source)


def _checked(token: str, source: str) -> str:
    if not token:
        raise ConfigurationError(f"{source} is empty")
    if any((c < " " and c != "\t") or c == "\x7f" for c in token):
        raise ConfigurationError(f"{source} holds a control character, which no HTTP header carries")
    return token


def _verify(ca_file: StrPath | None, env: Mapping[str, str]) -> ssl.SSLContext | bool:
    source = "ca_file"
    path = os.fspath(ca_file) if ca_file is not None else ""
    if not path:
        source, path = CA_FILE_ENV, env.get(CA_FILE_ENV, "")
    if not path:
        return True
    try:
        return ssl.create_default_context(cafile=path)
    except OSError as e:
        raise ConfigurationError(f"{source}: read the CA file {path}: {e.strerror}") from None
    except ssl.SSLError:
        raise ConfigurationError(f"{source}: the CA file {path} holds no certificate") from None
