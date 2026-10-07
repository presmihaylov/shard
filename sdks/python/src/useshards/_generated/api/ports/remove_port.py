from http import HTTPStatus
from typing import Any, cast
from urllib.parse import quote

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.error import Error
from ...types import UNSET, Response


def _get_kwargs(
    id: str,
    host_port: int,
) -> dict[str, Any]:
    _kwargs: dict[str, Any] = {
        "method": "delete",
        "url": "/v0/sandboxes/{id}/ports/{host_port}".format(
            id=quote(str(id), safe=""),
            host_port=quote(str(host_port), safe=""),
        ),
    }

    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Any | Error:
    if response.status_code == 204:
        response_204 = cast(Any, None)
        return response_204

    response_default = Error.from_dict(response.json())

    return response_default


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[Any | Error]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    id: str,
    host_port: int,
    *,
    client: AuthenticatedClient | Client,
) -> Response[Any | Error]:
    """Remove the forward on a host port"""

    kwargs = _get_kwargs(
        id=id,
        host_port=host_port,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    id: str,
    host_port: int,
    *,
    client: AuthenticatedClient | Client,
) -> Any | Error | None:
    """Remove the forward on a host port"""

    return sync_detailed(
        id=id,
        host_port=host_port,
        client=client,
    ).parsed


async def asyncio_detailed(
    id: str,
    host_port: int,
    *,
    client: AuthenticatedClient | Client,
) -> Response[Any | Error]:
    """Remove the forward on a host port"""

    kwargs = _get_kwargs(
        id=id,
        host_port=host_port,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    id: str,
    host_port: int,
    *,
    client: AuthenticatedClient | Client,
) -> Any | Error | None:
    """Remove the forward on a host port"""

    return (
        await asyncio_detailed(
            id=id,
            host_port=host_port,
            client=client,
        )
    ).parsed
