from __future__ import annotations

import json
from pathlib import Path

import pytest

from useshards._capture import OutputCapture
from useshards._config import resolve
from useshards._wire import STDERR, STDOUT
from useshards.errors import (
    APIError,
    ConfigurationError,
    ConflictError,
    NotFoundError,
    ServerError,
    UnsupportedError,
    api_error,
    failure_error,
)

ENV = {"SHARD_REMOTE": "https://shard.example.com", "SHARD_API_KEY": " shard_env "}


def test_env_defaults() -> None:
    settings = resolve(None, None, None, None, env=ENV)
    assert (settings.base_url, settings.api_key, settings.verify) == (
        "https://shard.example.com:443",
        "shard_env",
        True,
    )


def test_explicit_beats_env() -> None:
    settings = resolve("https://[::1]:8443/", "shard_arg", None, None, env=ENV)
    assert (settings.base_url, settings.api_key) == ("https://[::1]:8443", "shard_arg")


def test_repr_hides_the_key() -> None:
    assert "shard_env" not in repr(resolve(None, None, None, None, env=ENV))


@pytest.mark.parametrize(
    ("remote", "reason"),
    [
        ("http://shard.example.com", "must be an https url"),
        ("https://", "must be an https url"),
        ("https://shard.example.com/v0", "only a scheme, a host and a port"),
        ("https://user@shard.example.com", "only a scheme, a host and a port"),
        ("https://shard.example.com:99999", "not a number from 0 to 65535"),
    ],
)
def test_remote_refused(remote: str, reason: str) -> None:
    with pytest.raises(ConfigurationError, match=reason):
        resolve(remote, "k", None, None, env={})


def test_no_remote() -> None:
    with pytest.raises(ConfigurationError, match="no remote"):
        resolve(None, "k", None, None, env={})


def token_file(tmp_path: Path, text: str, mode: int = 0o600) -> Path:
    path = tmp_path / "token"
    path.write_text(text)
    path.chmod(mode)
    return path


def test_blank_key_falls_to_the_token_file(tmp_path: Path) -> None:
    path = token_file(tmp_path, json.dumps({"token": "shard_file", "scopes": ["*"]}))
    env = {**ENV, "SHARD_API_KEY": "  ", "SHARD_TOKEN_FILE": str(path)}
    assert resolve(None, None, None, None, env=env).api_key == "shard_file"


def test_token_file_others_can_read(tmp_path: Path) -> None:
    path = token_file(tmp_path, "shard_file\n", 0o644)
    with pytest.raises(ConfigurationError, match="mode 0644, which others can read") as caught:
        resolve(None, None, path, None, env=ENV)
    assert "shard_file" not in str(caught.value)


@pytest.mark.parametrize(("text", "reason"), [("", "holds no token"), ('{"token": ""}', "record .* holds no token")])
def test_token_file_without_a_token(tmp_path: Path, text: str, reason: str) -> None:
    with pytest.raises(ConfigurationError, match=reason):
        resolve(None, None, token_file(tmp_path, text), None, env=ENV)


def test_no_key() -> None:
    with pytest.raises(ConfigurationError, match="no API key"):
        resolve(None, None, None, None, env={"SHARD_REMOTE": "https://shard.example.com"})


def test_key_with_a_control_character() -> None:
    with pytest.raises(ConfigurationError, match="control character") as caught:
        resolve(None, "shard_\nsecret", None, None, env=ENV)
    assert "secret" not in str(caught.value)


def test_missing_ca_file(tmp_path: Path) -> None:
    with pytest.raises(ConfigurationError, match="SHARD_CA_FILE: read the CA file"):
        resolve(None, None, None, None, env={**ENV, "SHARD_CA_FILE": str(tmp_path / "missing.pem")})


def test_refusal_by_code_then_status() -> None:
    err = api_error(404, b'{"error":{"code":"not_found","message":"no sandbox sb"}}')
    assert isinstance(err, NotFoundError)
    assert (err.status, err.code, err.message, str(err)) == (
        404,
        "not_found",
        "no sandbox sb",
        "404 not_found: no sandbox sb",
    )
    assert isinstance(
        api_error(409, b'{"error":{"code":"unsupported","message":"sysbox has no pause"}}'), UnsupportedError
    )


def test_refusal_that_is_not_json() -> None:
    err = api_error(502, b"<html>bad gateway</html>" + b"x" * 1000)
    assert isinstance(err, ServerError)
    assert err.message.startswith("<html>bad gateway</html>")
    assert len(err.message) == 512
    blank = api_error(418, b"")
    assert type(blank) is APIError
    assert blank.message == "an empty body"


def test_stream_failure_takes_the_status_of_its_code() -> None:
    err = failure_error("in_use", "another client is attached")
    assert isinstance(err, ConflictError)
    assert err.status == 409
    assert isinstance(failure_error("no_such_code", "m"), ServerError)


def test_capture_keeps_the_newest_bytes_across_streams() -> None:
    capture = OutputCapture(5)
    capture.add(STDOUT, b"abc")
    capture.add(STDERR, b"de")
    capture.add(STDOUT, b"fg")
    assert capture.output() == (b"cfg", b"de")
    capture.add(STDERR, b"0123456789")
    assert capture.output() == (b"", b"56789")


def test_capture_off_and_refused() -> None:
    capture = OutputCapture(0)
    capture.add(STDOUT, b"abc")
    assert capture.output() == (b"", b"")
    with pytest.raises(ValueError, match="0 or more"):
        OutputCapture(-1)
