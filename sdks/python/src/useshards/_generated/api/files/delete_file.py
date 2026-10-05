from http import HTTPStatus
from typing import Any, cast
from urllib.parse import quote

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.error import Error
from ...types import UNSET, Response, Unset


def _get_kwargs(
    id: str,
    *,
    path: str,
    recursive: bool | Unset = UNSET,
) -> dict[str, Any]:
    params: dict[str, Any] = {}

    params["path"] = path

    params["recursive"] = recursive

    params = {k: v for k, v in params.items() if v is not UNSET and v is not None}

    _kwargs: dict[str, Any] = {
        "method": "delete",
        "url": "/v0/sandboxes/{id}/files".format(
            id=quote(str(id), safe=""),
        ),
        "params": params,
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
    *,
    client: AuthenticatedClient | Client,
    path: str,
    recursive: bool | Unset = UNSET,
) -> Response[Any | Error]:
    """Delete a path"""

    kwargs = _get_kwargs(
        id=id,
        path=path,
        recursive=recursive,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    id: str,
    *,
    client: AuthenticatedClient | Client,
    path: str,
    recursive: bool | Unset = UNSET,
) -> Any | Error | None:
    """Delete a path"""

    return sync_detailed(
        id=id,
        client=client,
        path=path,
        recursive=recursive,
    ).parsed


async def asyncio_detailed(
    id: str,
    *,
    client: AuthenticatedClient | Client,
    path: str,
    recursive: bool | Unset = UNSET,
) -> Response[Any | Error]:
    """Delete a path"""

    kwargs = _get_kwargs(
        id=id,
        path=path,
        recursive=recursive,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    id: str,
    *,
    client: AuthenticatedClient | Client,
    path: str,
    recursive: bool | Unset = UNSET,
) -> Any | Error | None:
    """Delete a path"""

    return (
        await asyncio_detailed(
            id=id,
            client=client,
            path=path,
            recursive=recursive,
        )
    ).parsed
