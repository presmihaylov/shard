from http import HTTPStatus
from typing import Any, cast
from urllib.parse import quote

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.error import Error
from ...models.record import Record
from ...types import UNSET, Response, Unset


def _get_kwargs(
    id: str,
    *,
    follow: bool | Unset = UNSET,
) -> dict[str, Any]:
    params: dict[str, Any] = {}

    params["follow"] = follow

    params = {k: v for k, v in params.items() if v is not UNSET and v is not None}

    _kwargs: dict[str, Any] = {
        "method": "get",
        "url": "/v0/sandboxes/{id}/egress-log".format(
            id=quote(str(id), safe=""),
        ),
        "params": params,
    }

    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Any | Error | list[Record]:
    if response.status_code == 101:
        response_101 = cast(Any, None)
        return response_101

    if response.status_code == 200:
        response_200 = []
        _response_200 = response.json()
        for response_200_item_data in _response_200:
            response_200_item = Record.from_dict(response_200_item_data)

            response_200.append(response_200_item)

        return response_200

    response_default = Error.from_dict(response.json())

    return response_default


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[Any | Error | list[Record]]:
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
    follow: bool | Unset = UNSET,
) -> Response[Any | Error | list[Record]]:
    """Read or follow the egress decisions of a sandbox"""

    kwargs = _get_kwargs(
        id=id,
        follow=follow,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    id: str,
    *,
    client: AuthenticatedClient | Client,
    follow: bool | Unset = UNSET,
) -> Any | Error | list[Record] | None:
    """Read or follow the egress decisions of a sandbox"""

    return sync_detailed(
        id=id,
        client=client,
        follow=follow,
    ).parsed


async def asyncio_detailed(
    id: str,
    *,
    client: AuthenticatedClient | Client,
    follow: bool | Unset = UNSET,
) -> Response[Any | Error | list[Record]]:
    """Read or follow the egress decisions of a sandbox"""

    kwargs = _get_kwargs(
        id=id,
        follow=follow,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    id: str,
    *,
    client: AuthenticatedClient | Client,
    follow: bool | Unset = UNSET,
) -> Any | Error | list[Record] | None:
    """Read or follow the egress decisions of a sandbox"""

    return (
        await asyncio_detailed(
            id=id,
            client=client,
            follow=follow,
        )
    ).parsed
