from http import HTTPStatus
from typing import Any, cast
from urllib.parse import quote

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.error import Error
from ...models.sandbox import Sandbox
from ...types import UNSET, Response


def _get_kwargs(
    id: str,
    name: str,
) -> dict[str, Any]:
    _kwargs: dict[str, Any] = {
        "method": "post",
        "url": "/v0/sandboxes/{id}/secrets/{name}".format(
            id=quote(str(id), safe=""),
            name=quote(str(name), safe=""),
        ),
    }

    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Error | Sandbox:
    if response.status_code == 200:
        response_200 = Sandbox.from_dict(response.json())

        return response_200

    response_default = Error.from_dict(response.json())

    return response_default


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[Error | Sandbox]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    id: str,
    name: str,
    *,
    client: AuthenticatedClient | Client,
) -> Response[Error | Sandbox]:
    """Grant a secret to a sandbox"""

    kwargs = _get_kwargs(
        id=id,
        name=name,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    id: str,
    name: str,
    *,
    client: AuthenticatedClient | Client,
) -> Error | Sandbox | None:
    """Grant a secret to a sandbox"""

    return sync_detailed(
        id=id,
        name=name,
        client=client,
    ).parsed


async def asyncio_detailed(
    id: str,
    name: str,
    *,
    client: AuthenticatedClient | Client,
) -> Response[Error | Sandbox]:
    """Grant a secret to a sandbox"""

    kwargs = _get_kwargs(
        id=id,
        name=name,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    id: str,
    name: str,
    *,
    client: AuthenticatedClient | Client,
) -> Error | Sandbox | None:
    """Grant a secret to a sandbox"""

    return (
        await asyncio_detailed(
            id=id,
            name=name,
            client=client,
        )
    ).parsed
