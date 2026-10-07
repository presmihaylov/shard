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
    name: str,
    *,
    follow: bool | Unset = UNSET,
) -> dict[str, Any]:
    params: dict[str, Any] = {}

    params["follow"] = follow

    params = {k: v for k, v in params.items() if v is not UNSET and v is not None}

    _kwargs: dict[str, Any] = {
        "method": "get",
        "url": "/v0/sandboxes/{id}/processes/{name}/logs".format(
            id=quote(str(id), safe=""),
            name=quote(str(name), safe=""),
        ),
        "params": params,
    }

    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Any | Error | str:
    if response.status_code == 101:
        response_101 = cast(Any, None)
        return response_101

    if response.status_code == 200:
        response_200 = response.text
        return response_200

    response_default = Error.from_dict(response.json())

    return response_default


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[Any | Error | str]:
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
    follow: bool | Unset = UNSET,
) -> Response[Any | Error | str]:
    """Read or follow the output of a process"""

    kwargs = _get_kwargs(
        id=id,
        name=name,
        follow=follow,
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
    follow: bool | Unset = UNSET,
) -> Any | Error | str | None:
    """Read or follow the output of a process"""

    return sync_detailed(
        id=id,
        name=name,
        client=client,
        follow=follow,
    ).parsed


async def asyncio_detailed(
    id: str,
    name: str,
    *,
    client: AuthenticatedClient | Client,
    follow: bool | Unset = UNSET,
) -> Response[Any | Error | str]:
    """Read or follow the output of a process"""

    kwargs = _get_kwargs(
        id=id,
        name=name,
        follow=follow,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    id: str,
    name: str,
    *,
    client: AuthenticatedClient | Client,
    follow: bool | Unset = UNSET,
) -> Any | Error | str | None:
    """Read or follow the output of a process"""

    return (
        await asyncio_detailed(
            id=id,
            name=name,
            client=client,
            follow=follow,
        )
    ).parsed
