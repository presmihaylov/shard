from http import HTTPStatus
from io import BytesIO
from typing import Any, cast
from urllib.parse import quote

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.error import Error
from ...types import UNSET, File, FileTypes, Response, Unset


def _get_kwargs(
    id: str,
    *,
    body: File,
    path: str,
    user: str | Unset = UNSET,
) -> dict[str, Any]:
    headers: dict[str, Any] = {}

    params: dict[str, Any] = {}

    params["path"] = path

    params["user"] = user

    params = {k: v for k, v in params.items() if v is not UNSET and v is not None}

    _kwargs: dict[str, Any] = {
        "method": "put",
        "url": "/v0/sandboxes/{id}/archive".format(
            id=quote(str(id), safe=""),
        ),
        "params": params,
    }

    _kwargs["content"] = body.payload
    headers["Content-Type"] = "application/x-tar"

    _kwargs["headers"] = headers
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
    body: File,
    path: str,
    user: str | Unset = UNSET,
) -> Response[Any | Error]:
    """Copy a directory into a running sandbox"""

    kwargs = _get_kwargs(
        id=id,
        body=body,
        path=path,
        user=user,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    id: str,
    *,
    client: AuthenticatedClient | Client,
    body: File,
    path: str,
    user: str | Unset = UNSET,
) -> Any | Error | None:
    """Copy a directory into a running sandbox"""

    return sync_detailed(
        id=id,
        client=client,
        body=body,
        path=path,
        user=user,
    ).parsed


async def asyncio_detailed(
    id: str,
    *,
    client: AuthenticatedClient | Client,
    body: File,
    path: str,
    user: str | Unset = UNSET,
) -> Response[Any | Error]:
    """Copy a directory into a running sandbox"""

    kwargs = _get_kwargs(
        id=id,
        body=body,
        path=path,
        user=user,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    id: str,
    *,
    client: AuthenticatedClient | Client,
    body: File,
    path: str,
    user: str | Unset = UNSET,
) -> Any | Error | None:
    """Copy a directory into a running sandbox"""

    return (
        await asyncio_detailed(
            id=id,
            client=client,
            body=body,
            path=path,
            user=user,
        )
    ).parsed
