"""The one HTTPX client every call rides, and every non-2xx answer raised as an APIError."""

from __future__ import annotations

from collections.abc import AsyncIterable, AsyncIterator
from contextlib import asynccontextmanager
from typing import Any

import httpx

from .._config import Settings
from .._version import __version__
from .._wire import connection_error
from ..errors import api_error

DEFAULT_TIMEOUT = 60.0
CHUNK = 1 << 20


class AsyncTransport:
    def __init__(self, settings: Settings, timeout: float | None) -> None:
        self.timeout = timeout
        self.http = httpx.AsyncClient(
            base_url=settings.base_url,
            headers={"Authorization": f"Bearer {settings.api_key}", "User-Agent": f"useshards-python/{__version__}"},
            verify=settings.verify,
            timeout=timeout,
        )

    def read_bound(self, read: float | None) -> httpx.Timeout:
        """The client's timeout with its read bound replaced, for a call that waits on the guest."""
        return httpx.Timeout(self.timeout, read=read)

    async def call(
        self,
        method: str,
        path: str,
        *,
        json: Any = None,
        params: dict[str, str] | None = None,
        content: bytes | AsyncIterable[bytes] | None = None,
        headers: dict[str, str] | None = None,
        timeout: httpx.Timeout | None = None,
    ) -> httpx.Response:
        request = self.http.build_request(
            method,
            path,
            json=json,
            params=params,
            content=content,
            headers=headers,
            timeout=timeout if timeout is not None else self.http.timeout,
        )
        try:
            response = await self.http.send(request)
        except httpx.TransportError as e:
            raise connection_error(request, e) from e
        if response.is_success:
            return response
        raise api_error(response.status_code, response.content)

    @asynccontextmanager
    async def stream(
        self, method: str, path: str, *, params: dict[str, str] | None = None
    ) -> AsyncIterator[httpx.Response]:
        """A response whose body the caller reads as it arrives; a non-2xx answer raises once its body is in."""
        request = self.http.build_request(method, path, params=params)
        try:
            response = await self.http.send(request, stream=True)
        except httpx.TransportError as e:
            raise connection_error(request, e) from e
        try:
            if not response.is_success:
                try:
                    await response.aread()
                except httpx.TransportError as e:
                    raise connection_error(request, e) from e
                raise api_error(response.status_code, response.content)
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
