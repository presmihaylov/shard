from __future__ import annotations

import asyncio
import warnings
from pathlib import Path

import pytest

from useshards import AsyncShard, Shard
from useshards._capture import OutputCapture
from useshards._config import PLAIN_WARNING, resolve
from useshards._wire import STDERR, STDOUT
from useshards.errors import (
    APIError,
    CommandNotStartedError,
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
    settings = resolve(None, None, None, env=ENV)
    assert (settings.base_url, settings.api_key, settings.verify) == (
        "https://shard.example.com:443",
        "shard_env",
        True,
    )


def test_explicit_beats_env() -> None:
    settings = resolve("https://[::1]:8443/", "shard_arg", None, env=ENV)
    assert (settings.base_url, settings.api_key) == ("https://[::1]:8443", "shard_arg")


def test_repr_hides_the_key() -> None:
    assert "shard_env" not in repr(resolve(None, None, None, env=ENV))


@pytest.mark.parametrize(
    ("remote", "reason"),
    [
        ("ftp://shard.example.com", "must be an http or https url"),
        ("https://", "must be an http or https url"),
        ("https://shard.example.com/v0", "only a scheme, a host and a port"),
        ("https://user@shard.example.com", "only a scheme, a host and a port"),
        ("https://shard.example.com:99999", "not a number from 0 to 65535"),
    ],
)
def test_remote_refused(remote: str, reason: str) -> None:
    with pytest.raises(ConfigurationError, match=reason):
        resolve(remote, "k", None, env={})


# A broken bracketed host raised a bare ValueError that quoted part of the value, or became port 80 (SHARD-631).
@pytest.mark.parametrize("remote", ["http://[::1", "https://[::1", "http://[", "http://[zz]:80", "http://[::1]x"])
@pytest.mark.parametrize("source", ["remote", "SHARD_REMOTE"])
def test_a_broken_host_is_refused(remote: str, source: str) -> None:
    arg, env = (remote, {}) if source == "remote" else (None, {"SHARD_REMOTE": remote})
    with pytest.raises(ConfigurationError, match=f"^{source} must be an http or https url with a host") as caught:
        resolve(arg, "k", None, env=env)
    assert remote not in str(caught.value)
    assert "zz" not in str(caught.value)


def test_no_remote() -> None:
    with pytest.raises(ConfigurationError, match="no remote"):
        resolve(None, "k", None, env={})


@pytest.mark.parametrize(
    ("remote", "base_url"),
    [
        ("http://shard.example.com", "http://shard.example.com:80"),
        ("http://127.0.0.1:8080/", "http://127.0.0.1:8080"),
        ("https://shard.example.com", "https://shard.example.com:443"),
    ],
)
def test_either_scheme_takes_its_port(remote: str, base_url: str) -> None:
    settings = resolve(remote, "k", None, env={})
    assert (settings.base_url, settings.plain, settings.verify) == (base_url, remote.startswith("http:"), True)


@pytest.mark.parametrize(
    ("ca_file", "env", "source"), [("ca.pem", {}, "ca_file"), (None, {"SHARD_CA_FILE": "ca.pem"}, "SHARD_CA_FILE")]
)
def test_a_ca_with_http_is_refused(ca_file: str | None, env: dict[str, str], source: str) -> None:
    remote = "http://shard.example.com:80"
    reason = f"{source} is set, and the remote {remote} is http: a CA certificate verifies an https remote only"
    with pytest.raises(ConfigurationError) as caught:
        resolve("http://shard.example.com", "k", ca_file, env=env)
    assert str(caught.value) == reason


def test_http_warns_once_per_client_with_the_cli_text() -> None:
    with warnings.catch_warnings(record=True) as caught:
        warnings.simplefilter("always")
        clients = [Shard(remote, "k") for remote in ("http://127.0.0.1:1", "http://127.0.0.1:1", "https://127.0.0.1:1")]
        async_client = AsyncShard("http://127.0.0.1:1", "k")
    for client in clients:
        client.close()
    asyncio.run(async_client.aclose())
    assert [str(w.message) for w in caught] == [PLAIN_WARNING] * 3
    assert {w.filename for w in caught} == {__file__}
    assert PLAIN_WARNING == (
        "HTTP does not encrypt this connection. Use it only on localhost or through a trusted encrypted network."
    )


def test_no_key() -> None:
    with pytest.raises(ConfigurationError, match="no API key"):
        resolve(None, None, None, env={"SHARD_REMOTE": "https://shard.example.com"})


def test_key_with_a_control_character() -> None:
    with pytest.raises(ConfigurationError, match="control character") as caught:
        resolve(None, "shard_\nsecret", None, env=ENV)
    assert "secret" not in str(caught.value)


def test_missing_ca_file(tmp_path: Path) -> None:
    with pytest.raises(ConfigurationError, match="SHARD_CA_FILE: read the CA file"):
        resolve(None, None, None, env={**ENV, "SHARD_CA_FILE": str(tmp_path / "missing.pem")})


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


def test_a_command_that_never_started_keeps_its_shell_code() -> None:
    body = b'{"error":{"code":"command_not_started","message":"sandbox web could not run \\"/x\\"","exit_code":126}}'
    err = api_error(422, body)
    assert isinstance(err, CommandNotStartedError)
    assert (err.exit_code, err.reason) == (126, 'sandbox web could not run "/x"')
    assert type(api_error(422, b'{"error":{"code":"command_not_started","message":"m"}}')) is APIError


def test_stream_failure_takes_the_status_of_its_code() -> None:
    err = failure_error("in_use", "another client is attached")
    assert isinstance(err, ConflictError)
    assert err.status == 409
    assert isinstance(failure_error("no_such_code", "m"), ServerError)
    assert failure_error("timeout", "the provider did not answer").status == 504


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
