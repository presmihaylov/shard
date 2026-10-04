from http import HTTPStatus
from typing import Any, cast
from urllib.parse import quote

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.entries_response import EntriesResponse
from ...models.error import Error
from ...types import UNSET, Response, Unset


def _get_kwargs(
    id: str,
    *,
    path: str | Unset = UNSET,
) -> dict[str, Any]:
    params: dict[str, Any] = {}

    params["path"] = path

    params = {k: v for k, v in params.items() if v is not UNSET and v is not None}

    _kwargs: dict[str, Any] = {
        "method": "get",
        "url": "/v0/sandboxes/{id}/ls".format(
            id=quote(str(id), safe=""),
        ),
        "params": params,
    }

    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> EntriesResponse | Error:
    if response.status_code == 200:
        response_200 = EntriesResponse.from_dict(response.json())

        return response_200

    response_default = Error.from_dict(response.json())

    return response_default


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[EntriesResponse | Error]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    id: str,
    *,
    client: AuthenticatedClient | Client,
    path: str | Unset = UNSET,
) -> Response[EntriesResponse | Error]:
    """List a directory"""

    kwargs = _get_kwargs(
        id=id,
        path=path,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    id: str,
    *,
    client: AuthenticatedClient | Client,
    path: str | Unset = UNSET,
) -> EntriesResponse | Error | None:
    """List a directory"""

    return sync_detailed(
        id=id,
        client=client,
        path=path,
    ).parsed


async def asyncio_detailed(
    id: str,
    *,
    client: AuthenticatedClient | Client,
    path: str | Unset = UNSET,
) -> Response[EntriesResponse | Error]:
    """List a directory"""

    kwargs = _get_kwargs(
        id=id,
        path=path,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    id: str,
    *,
    client: AuthenticatedClient | Client,
    path: str | Unset = UNSET,
) -> EntriesResponse | Error | None:
    """List a directory"""

    return (
        await asyncio_detailed(
            id=id,
            client=client,
            path=path,
        )
    ).parsed
