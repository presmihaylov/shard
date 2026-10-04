"""The one HTTPX client every call rides, the generated client over it, and every non-2xx answer as an APIError."""

from __future__ import annotations

import contextvars
import functools
from collections.abc import AsyncIterable, AsyncIterator, Callable, Sequence
from contextlib import asynccontextmanager
from typing import Any, Protocol, TypeVar

import httpx

from .._config import Settings
from .._generated.client import Client
from .._generated.types import UNSET, Response, Unset
from .._version import __version__
from .._wire import AsyncCall, AsyncFetch, connection_error
from ..errors import ProtocolError, api_error

DEFAULT_TIMEOUT = 60.0
CHUNK = 1 << 20

M = TypeVar("M")
R = TypeVar("R")

# A generated call takes no timeout, so a call that waits on the guest leaves its bound here for the request hook.
_bound: contextvars.ContextVar[httpx.Timeout | None] = contextvars.ContextVar("useshards_timeout", default=None)


class Page(Protocol):
    @property
    def next_(self) -> None | str: ...


P = TypeVar("P", bound=Page)


class AsyncTransport:
    def __init__(
        self, settings: Settings, timeout: float | None, transport: httpx.AsyncBaseTransport | None = None
    ) -> None:
        self.timeout = timeout
        self.http = httpx.AsyncClient(
            base_url=settings.base_url,
            transport=transport,
            headers={"Authorization": f"Bearer {settings.api_key}", "User-Agent": f"useshards-python/{__version__}"},
            verify=settings.verify,
            timeout=timeout,
            event_hooks={"request": [_bind], "response": [_refuse]},
        )
        self.api = Client(base_url=settings.base_url).set_async_httpx_client(self.http)

    def read_bound(self, read: float | None) -> httpx.Timeout:
        """The client's timeout with its read bound replaced, for a call that waits on the guest."""
        return httpx.Timeout(self.timeout, read=read)

    async def send(self, call: AsyncCall, timeout: httpx.Timeout | None = None) -> Response[Any]:
        """Run a generated call; a dropped connection is a ShardConnectionError, an unreadable body a ProtocolError."""
        token = _bound.set(timeout)
        try:
            return await call()
        except httpx.TransportError as e:
            raise connection_error(e.request, e) from e
        except (KeyError, TypeError, ValueError) as e:
            raise ProtocolError(f"the daemon answered a body the SDK cannot read: {e!r}") from None
        finally:
            _bound.reset(token)

    async def answer(self, kind: type[M], call: AsyncCall, timeout: httpx.Timeout | None = None) -> M:
        """The model a generated call parsed from its 2xx answer."""
        parsed = (await self.send(call, timeout)).parsed
        if not isinstance(parsed, kind):
            raise ProtocolError(f"the daemon answered {type(parsed).__name__} where {kind.__name__} belongs")
        return parsed

    async def listed(self, kind: type[P], fetch: AsyncFetch, rows: Callable[[P], Sequence[R]]) -> list[R]:
        """Every row of a paged list, one page after another."""
        out: list[R] = []
        cursor: str | Unset = UNSET
        while True:
            page = await self.answer(kind, functools.partial(fetch, cursor))
            out.extend(rows(page))
            if not page.next_:
                return out
            cursor = page.next_

    async def put(self, path: str, params: dict[str, str], content: bytes | AsyncIterable[bytes], size: int) -> None:
        """A body of a known length; the generated client takes a file it buffers, never a stream."""
        # An explicit length keeps HTTPX from sending the stream chunked, which the daemon refuses.
        request = self.http.build_request(
            "PUT", path, params=params, content=content, headers={"Content-Length": str(size)}
        )
        try:
            await self.http.send(request)
        except httpx.TransportError as e:
            raise connection_error(request, e) from e

    @asynccontextmanager
    async def stream(self, path: str, params: dict[str, str]) -> AsyncIterator[httpx.Response]:
        """A GET whose body the caller reads as it arrives; the generated client buffers the whole body."""
        request = self.http.build_request("GET", path, params=params)
        try:
            response = await self.http.send(request, stream=True)
        except httpx.TransportError as e:
            raise connection_error(request, e) from e
        try:
            yield response
        finally:
            await response.aclose()

    async def aclose(self) -> None:
        await self.http.aclose()


async def body(response: httpx.Response) -> AsyncIterator[bytes]:
    """A streamed body as it arrives, a cut named as the dropped call it is."""
    try:
        async for chunk in response.aiter_bytes(CHUNK):
            yield chunk
    except httpx.TransportError as e:
        raise connection_error(response.request, e) from e


async def _bind(request: httpx.Request) -> None:
    timeout = _bound.get()
    if timeout is not None:
        request.extensions["timeout"] = timeout.as_dict()


async def _refuse(response: httpx.Response) -> None:
    """Raise every answer but a 2xx or a WebSocket upgrade, once the daemon's error body is in."""
    if response.is_success or response.status_code == 101:
        return
    await response.aread()
    raise api_error(response.status_code, response.content)
