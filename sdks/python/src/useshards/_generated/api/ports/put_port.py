from http import HTTPStatus
from typing import Any, cast
from urllib.parse import quote

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.error import Error
from ...models.port import Port
from ...models.port_request import PortRequest
from ...types import UNSET, Response


def _get_kwargs(
    id: str,
    host_port: int,
    *,
    body: PortRequest,
) -> dict[str, Any]:
    headers: dict[str, Any] = {}

    _kwargs: dict[str, Any] = {
        "method": "put",
        "url": "/v0/sandboxes/{id}/ports/{host_port}".format(
            id=quote(str(id), safe=""),
            host_port=quote(str(host_port), safe=""),
        ),
    }

    _kwargs["json"] = body.to_dict()

    headers["Content-Type"] = "application/json"

    _kwargs["headers"] = headers
    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Error | Port:
    if response.status_code == 200:
        response_200 = Port.from_dict(response.json())

        return response_200

    response_default = Error.from_dict(response.json())

    return response_default


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[Error | Port]:
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
    body: PortRequest,
) -> Response[Error | Port]:
    """Forward a host port into a sandbox, or change that forward"""

    kwargs = _get_kwargs(
        id=id,
        host_port=host_port,
        body=body,
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
    body: PortRequest,
) -> Error | Port | None:
    """Forward a host port into a sandbox, or change that forward"""

    return sync_detailed(
        id=id,
        host_port=host_port,
        client=client,
        body=body,
    ).parsed


async def asyncio_detailed(
    id: str,
    host_port: int,
    *,
    client: AuthenticatedClient | Client,
    body: PortRequest,
) -> Response[Error | Port]:
    """Forward a host port into a sandbox, or change that forward"""

    kwargs = _get_kwargs(
        id=id,
        host_port=host_port,
        body=body,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    id: str,
    host_port: int,
    *,
    client: AuthenticatedClient | Client,
    body: PortRequest,
) -> Error | Port | None:
    """Forward a host port into a sandbox, or change that forward"""

    return (
        await asyncio_detailed(
            id=id,
            host_port=host_port,
            client=client,
            body=body,
        )
    ).parsed
