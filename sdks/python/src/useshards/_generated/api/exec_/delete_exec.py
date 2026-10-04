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
    exec_: str,
) -> dict[str, Any]:
    _kwargs: dict[str, Any] = {
        "method": "delete",
        "url": "/v0/sandboxes/{id}/exec/{exec_}".format(
            id=quote(str(id), safe=""),
            exec_=quote(str(exec_), safe=""),
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
    exec_: str,
    *,
    client: AuthenticatedClient | Client,
) -> Response[Any | Error]:
    """Forget an exec that ended"""

    kwargs = _get_kwargs(
        id=id,
        exec_=exec_,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    id: str,
    exec_: str,
    *,
    client: AuthenticatedClient | Client,
) -> Any | Error | None:
    """Forget an exec that ended"""

    return sync_detailed(
        id=id,
        exec_=exec_,
        client=client,
    ).parsed


async def asyncio_detailed(
    id: str,
    exec_: str,
    *,
    client: AuthenticatedClient | Client,
) -> Response[Any | Error]:
    """Forget an exec that ended"""

    kwargs = _get_kwargs(
        id=id,
        exec_=exec_,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    id: str,
    exec_: str,
    *,
    client: AuthenticatedClient | Client,
) -> Any | Error | None:
    """Forget an exec that ended"""

    return (
        await asyncio_detailed(
            id=id,
            exec_=exec_,
            client=client,
        )
    ).parsed
