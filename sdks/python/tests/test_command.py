from __future__ import annotations

import asyncio
import json
import threading
import time
from collections.abc import Iterator

import pytest
from fakedaemon import IN_USE, NOT_STARTED, OP_PING, OP_PONG, RECORD, FakeDaemon, Peer

from useshards._async import _command as async_command
from useshards._async._transport import AsyncTransport
from useshards._async._ws import AsyncWebSocket
from useshards._config import Settings
from useshards._sync import _command
from useshards._sync._transport import Transport
from useshards._sync._ws import WebSocket
from useshards._wire import EXIT, STDERR, STDOUT
from useshards.errors import CommandNotStartedError, ConflictError, ProtocolError, ShardConnectionError

LIMIT = 1 << 20


@pytest.fixture
def daemon() -> Iterator[FakeDaemon]:
    fake = FakeDaemon()
    yield fake
    fake.close()
    assert fake.errors == []


def transport(daemon: FakeDaemon) -> Transport:
    return Transport(Settings(base_url=daemon.url, api_key="k", verify=True), 5.0)


def kills(daemon: FakeDaemon) -> list[bytes]:
    return [body for method, path, body in daemon.requests if path.endswith("/kill")]


def test_run_splits_interleaved_output(daemon: FakeDaemon) -> None:
    def session(peer: Peer) -> str:
        for stream, data in [(STDOUT, b"a"), (STDERR, b"b"), (STDOUT, b"c"), (STDERR, b"d")]:
            peer.send(stream, data)
        return peer.finish(3)

    daemon.attaches = [session]
    seen: list[tuple[str, bytes]] = []
    result = _command.run_command(
        transport(daemon),
        "sb",
        "true",
        output_limit_bytes=LIMIT,
        on_stdout=lambda d: seen.append(("out", d)),
        on_stderr=lambda d: seen.append(("err", d)),
    )
    assert (result.exit_code, result.stdout, result.stderr) == (3, "ac", "bd")
    assert seen == [("out", b"a"), ("err", b"b"), ("out", b"c"), ("err", b"d")]
    assert daemon.outcomes == ["close"]
    start = json.loads(daemon.requests[0][2])
    assert start == {"command": ["/bin/sh", "-c", "true"], "stdin": False, "tty": False, "attach": True}


def test_capture_keeps_the_newest_bytes_and_callbacks_see_all(daemon: FakeDaemon) -> None:
    def session(peer: Peer) -> str:
        peer.send(STDOUT, b"abc")
        peer.send(STDERR, b"def")
        return peer.finish(0, lost_bytes=9)

    daemon.attaches = [session]
    seen: list[bytes] = []
    result = _command.run_command(transport(daemon), "sb", "true", output_limit_bytes=4, on_stdout=seen.append)
    assert (result.stdout, result.stderr, result.lost_bytes) == ("c", "def", 9)
    assert seen == [b"abc"]


def test_capture_off_still_calls_back(daemon: FakeDaemon) -> None:
    def session(peer: Peer) -> str:
        peer.send(STDOUT, b"out")
        peer.send(STDERR, b"err")
        return peer.finish(0)

    daemon.attaches = [session]
    seen: list[bytes] = []
    result = _command.run_command(
        transport(daemon), "sb", "true", output_limit_bytes=0, on_stdout=seen.append, on_stderr=seen.append
    )
    assert (result.stdout_bytes, result.stderr_bytes) == (b"", b"")
    assert seen == [b"out", b"err"]


def test_run_sends_stdin_in_pieces(daemon: FakeDaemon) -> None:
    data = bytes(range(256)) * 512 + b"tail!"

    def session(peer: Peer) -> list[bytes]:
        pieces = peer.stdin()
        peer.finish(0)
        return pieces

    daemon.attaches = [session]
    _command.run_command(transport(daemon), "sb", ["cat"], stdin=data, output_limit_bytes=LIMIT)
    (pieces,) = daemon.outcomes
    assert [len(p) for p in pieces] == [_command.STDIN_PIECE, _command.STDIN_PIECE, 5]
    assert b"".join(pieces) == data
    assert json.loads(daemon.requests[0][2])["stdin"] is True


def test_exit_with_an_error_never_started(daemon: FakeDaemon) -> None:
    daemon.attaches = [lambda peer: peer.finish(127, error="exec: no such file")]
    with pytest.raises(CommandNotStartedError, match="no such file") as caught:
        _command.run_command(transport(daemon), "sb", ["nope"], output_limit_bytes=LIMIT)
    assert caught.value.exit_code == 127


def test_refused_start_never_attaches(daemon: FakeDaemon) -> None:
    daemon.create = NOT_STARTED
    with pytest.raises(CommandNotStartedError, match="could not run") as caught:
        _command.run_command(transport(daemon), "sb", ["/no/such"], output_limit_bytes=LIMIT)
    assert caught.value.exit_code == 127
    assert [method for method, _, _ in daemon.requests] == ["POST"]


def test_failure_is_the_refusal_it_names(daemon: FakeDaemon) -> None:
    daemon.attaches = [lambda peer: peer.fail("sandbox_not_running", "sandbox sb is stopped")]
    with pytest.raises(ConflictError, match="sandbox sb is stopped") as caught:
        _command.run_command(transport(daemon), "sb", "true", output_limit_bytes=LIMIT)
    assert (caught.value.status, caught.value.code) == (409, "sandbox_not_running")


def test_cut_asks_the_record_why(daemon: FakeDaemon) -> None:
    daemon.attaches = [lambda peer: peer.send(STDOUT, b"x")]
    daemon.record = (200, {**RECORD, "lost_bytes": 7})
    with pytest.raises(ShardConnectionError, match="the command is running with 7 bytes of output lost"):
        _command.run_command(transport(daemon), "sb", "true", output_limit_bytes=LIMIT)


def test_cut_with_no_record_names_the_drop(daemon: FakeDaemon) -> None:
    daemon.attaches = [lambda peer: None]
    daemon.record = (500, {"error": {"code": "internal", "message": "store down"}})
    with pytest.raises(ShardConnectionError, match="ended without an exit status: the stream to the daemon dropped"):
        _command.run_command(transport(daemon), "sb", "true", output_limit_bytes=LIMIT)


def test_attach_retries_while_in_use(daemon: FakeDaemon) -> None:
    daemon.attaches = [IN_USE, IN_USE, lambda peer: peer.finish(0)]
    result = _command.run_command(transport(daemon), "sb", "true", output_limit_bytes=LIMIT)
    assert result.exit_code == 0


def test_attach_in_use_past_the_bound(daemon: FakeDaemon, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(_command, "_REATTACH_BOUND", 0.2)
    daemon.refuse_when_out = IN_USE
    with pytest.raises(ConflictError) as caught:
        _command.run_command(transport(daemon), "sb", "true", output_limit_bytes=LIMIT)
    assert caught.value.code == "in_use"


def test_handshake_with_the_wrong_accept(daemon: FakeDaemon) -> None:
    daemon.bad_accept = True
    daemon.attaches = [lambda peer: peer.until_end()]
    with pytest.raises(ProtocolError, match="key that does not match"):
        _command.run_command(transport(daemon), "sb", "true", output_limit_bytes=LIMIT)


def test_ping_is_answered(daemon: FakeDaemon) -> None:
    def session(peer: Peer) -> tuple[int, bytes] | None:
        peer.frame(OP_PING, b"are you there")
        pong = peer.recv()
        peer.finish(0)
        return pong

    daemon.attaches = [session]
    _command.run_command(transport(daemon), "sb", "true", output_limit_bytes=LIMIT)
    assert daemon.outcomes == [(OP_PONG, b"are you there")]


def test_disconnect_leaves_the_command_and_wait_replays(daemon: FakeDaemon) -> None:
    def first(peer: Peer) -> str:
        peer.send(STDOUT, b"one")
        return peer.until_end()

    def replay(peer: Peer) -> str:
        peer.send(STDOUT, b"one")
        return peer.finish(0)

    daemon.attaches = [first, replay]
    seen: list[bytes] = []
    handle = _command.start_command(transport(daemon), "sb", "true", output_limit_bytes=LIMIT, on_stdout=seen.append)
    handle.disconnect()
    result = handle.wait()
    assert result.stdout == "one"
    assert seen == [b"one", b"one"]
    assert daemon.outcomes == ["close", "close"]
    assert kills(daemon) == []


def test_wait_timeout_keeps_the_stream(daemon: FakeDaemon) -> None:
    go = threading.Event()

    def session(peer: Peer) -> str:
        assert go.wait(5)
        return peer.finish(0)

    daemon.attaches = [session]
    handle = _command.start_command(transport(daemon), "sb", "true", output_limit_bytes=LIMIT)
    with pytest.raises(TimeoutError):
        handle.wait(timeout=0.1)
    go.set()
    assert handle.wait().exit_code == 0
    assert len(daemon.outcomes) == 1


def test_kill_sends_the_signal(daemon: FakeDaemon) -> None:
    daemon.attaches = [lambda peer: peer.finish(0)]
    handle = _command.start_command(transport(daemon), "sb", "true", output_limit_bytes=LIMIT)
    handle.kill("KILL")
    assert [json.loads(body) for body in kills(daemon)] == [{"signal": "KILL"}]
    handle.wait()


def test_write_stdin_needs_stdin(daemon: FakeDaemon) -> None:
    daemon.attaches = [lambda peer: peer.finish(0)]
    handle = _command.start_command(transport(daemon), "sb", "true", output_limit_bytes=LIMIT)
    handle.wait()
    with pytest.raises(ValueError, match="without stdin=True"):
        handle.write_stdin("x")


def read_stdin(peer: Peer) -> list[bytes]:
    pieces = peer.stdin()
    peer.finish(0)
    return pieces


def test_concurrent_writes_land_whole(daemon: FakeDaemon, monkeypatch: pytest.MonkeyPatch) -> None:
    send = WebSocket.send_binary

    def slow(ws: WebSocket, payload: bytes) -> None:
        # Room for the other writer to cut in between pieces.
        time.sleep(0.01)
        send(ws, payload)

    monkeypatch.setattr(WebSocket, "send_binary", slow)
    daemon.attaches = [read_stdin]
    handle = _command.start_command(transport(daemon), "sb", "cat", stdin=True, output_limit_bytes=LIMIT)
    a, b = b"a" * (2 * _command.STDIN_PIECE + 1), b"b" * (2 * _command.STDIN_PIECE + 1)
    writers = [threading.Thread(target=handle.write_stdin, args=(data,)) for data in (a, b)]
    for writer in writers:
        writer.start()
    for writer in writers:
        writer.join()
    handle.close_stdin()
    handle.wait()
    (pieces,) = daemon.outcomes
    assert b"".join(pieces) in (a + b, b + a)


def test_async_writes_and_close_land_whole(daemon: FakeDaemon, monkeypatch: pytest.MonkeyPatch) -> None:
    send = AsyncWebSocket.send_binary

    async def yielding(ws: AsyncWebSocket, payload: bytes) -> None:
        await asyncio.sleep(0)
        await send(ws, payload)

    monkeypatch.setattr(AsyncWebSocket, "send_binary", yielding)
    daemon.attaches = [read_stdin]
    a, b = b"a" * (2 * _command.STDIN_PIECE + 1), b"b" * (2 * _command.STDIN_PIECE + 1)

    async def run() -> None:
        client = AsyncTransport(Settings(base_url=daemon.url, api_key="k", verify=True), 5.0)
        handle = await async_command.start_command(client, "sb", "cat", stdin=True, output_limit_bytes=LIMIT)
        await asyncio.gather(handle.write_stdin(a), handle.write_stdin(b), handle.close_stdin())
        await handle.wait()
        await client.aclose()

    asyncio.run(run())
    (pieces,) = daemon.outcomes
    assert b"".join(pieces) == a + b


def test_write_stdin_on_a_terminal_needs_no_stdin(daemon: FakeDaemon) -> None:
    def session(peer: Peer) -> list[bytes]:
        pieces = peer.stdin()
        peer.finish(0)
        return pieces

    daemon.attaches = [session]
    handle = _command.start_command(transport(daemon), "sb", "sh", tty=True, output_limit_bytes=LIMIT)
    handle.write_stdin("x")
    handle.close_stdin()
    handle.wait()
    assert daemon.outcomes == [[b"x"]]


def test_negative_limit_refused_before_the_start(daemon: FakeDaemon) -> None:
    with pytest.raises(ValueError, match="0 or more"):
        _command.run_command(transport(daemon), "sb", "true", output_limit_bytes=-1)
    assert daemon.requests == []


def test_cancel_leaves_the_command_running(daemon: FakeDaemon) -> None:
    def session(peer: Peer) -> str:
        peer.send(STDOUT, b"x")
        return peer.until_end()

    daemon.attaches = [session]

    async def run() -> None:
        client = AsyncTransport(Settings(base_url=daemon.url, api_key="k", verify=True), 5.0)
        got = asyncio.Event()
        task = asyncio.create_task(
            async_command.run_command(client, "sb", "sleep 60", output_limit_bytes=LIMIT, on_stdout=lambda d: got.set())
        )
        await asyncio.wait_for(got.wait(), 5)
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task
        await client.aclose()

    asyncio.run(run())
    daemon.close()
    assert daemon.outcomes == ["eof"]
    assert kills(daemon) == []


def test_async_background_wait(daemon: FakeDaemon) -> None:
    def session(peer: Peer) -> str:
        peer.send(STDERR, b"warn")
        return peer.finish(2)

    daemon.attaches = [session]

    async def run() -> tuple[int, str]:
        client = AsyncTransport(Settings(base_url=daemon.url, api_key="k", verify=True), 5.0)
        handle = await async_command.start_command(client, "sb", "true", output_limit_bytes=LIMIT)
        result = await handle.wait()
        await client.aclose()
        return result.exit_code, result.stderr

    assert asyncio.run(run()) == (2, "warn")


def bad_frame_after_the_exit(peer: Peer) -> str:
    peer.send(EXIT, b'{"code":0}')
    peer.recv()
    peer.frame(0x3, b"invalid opcode")
    return peer.until_end()


def test_a_fault_after_the_exit_reaches_wait(daemon: FakeDaemon) -> None:
    daemon.attaches = [bad_frame_after_the_exit]
    handle = _command.start_command(transport(daemon), "sb", "true", output_limit_bytes=LIMIT)
    with pytest.raises(ProtocolError, match="unknown opcode 3"):
        handle.wait()
    daemon.close()
    assert daemon.outcomes == ["eof"]


def test_async_fault_after_the_exit_reaches_wait(daemon: FakeDaemon) -> None:
    daemon.attaches = [bad_frame_after_the_exit]

    async def run() -> None:
        client = AsyncTransport(Settings(base_url=daemon.url, api_key="k", verify=True), 5.0)
        handle = await async_command.start_command(client, "sb", "true", output_limit_bytes=LIMIT)
        with pytest.raises(ProtocolError, match="unknown opcode 3"):
            await handle.wait()
        await client.aclose()

    asyncio.run(run())
    daemon.close()
    assert daemon.outcomes == ["eof"]


def test_exit_wins_over_unsent_input(daemon: FakeDaemon) -> None:
    returned = threading.Event()

    def session(peer: Peer) -> bool:
        peer.finish_without_close(0)
        return returned.wait(5)

    daemon.attaches = [session]
    result = _command.run_command(
        transport(daemon), "sb", ["head", "-c0"], stdin=b"x" * (16 << 20), output_limit_bytes=LIMIT
    )
    returned.set()
    assert result.exit_code == 0
    daemon.close()
    assert daemon.outcomes == [True]


def fault(self: WebSocket, payload: bytes) -> None:
    raise RuntimeError("a local fault")


def test_unrelated_write_error_rejects_at_once(daemon: FakeDaemon, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(WebSocket, "send_binary", fault)
    daemon.attaches = [lambda peer: peer.until_end()]
    with pytest.raises(RuntimeError, match="a local fault"):
        _command.run_command(transport(daemon), "sb", ["cat"], stdin=b"x", output_limit_bytes=LIMIT)
    daemon.close()
    assert daemon.outcomes == ["eof"]
    assert kills(daemon) == []


def test_exit_never_hides_an_unrelated_write_error(daemon: FakeDaemon, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(WebSocket, "send_binary", fault)
    daemon.attaches = [lambda peer: peer.finish(0)]
    with pytest.raises(RuntimeError, match="a local fault"):
        _command.run_command(transport(daemon), "sb", ["cat"], stdin=b"x", output_limit_bytes=LIMIT)
