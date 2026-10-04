from http import HTTPStatus
from typing import Any, cast
from urllib.parse import quote

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.error import Error
from ...models.exec_ import Exec
from ...types import UNSET, Response, Unset


def _get_kwargs(
    id: str,
    exec_: str,
    *,
    wait: bool | Unset = UNSET,
) -> dict[str, Any]:
    params: dict[str, Any] = {}

    params["wait"] = wait

    params = {k: v for k, v in params.items() if v is not UNSET and v is not None}

    _kwargs: dict[str, Any] = {
        "method": "get",
        "url": "/v0/sandboxes/{id}/exec/{exec_}".format(
            id=quote(str(id), safe=""),
            exec_=quote(str(exec_), safe=""),
        ),
        "params": params,
    }

    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Any | Error | Exec:
    if response.status_code == 101:
        response_101 = cast(Any, None)
        return response_101

    if response.status_code == 200:
        response_200 = Exec.from_dict(response.json())

        return response_200

    response_default = Error.from_dict(response.json())

    return response_default


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[Any | Error | Exec]:
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
    wait: bool | Unset = UNSET,
) -> Response[Any | Error | Exec]:
    """Read, wait for or attach to an exec"""

    kwargs = _get_kwargs(
        id=id,
        exec_=exec_,
        wait=wait,
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
    wait: bool | Unset = UNSET,
) -> Any | Error | Exec | None:
    """Read, wait for or attach to an exec"""

    return sync_detailed(
        id=id,
        exec_=exec_,
        client=client,
        wait=wait,
    ).parsed


async def asyncio_detailed(
    id: str,
    exec_: str,
    *,
    client: AuthenticatedClient | Client,
    wait: bool | Unset = UNSET,
) -> Response[Any | Error | Exec]:
    """Read, wait for or attach to an exec"""

    kwargs = _get_kwargs(
        id=id,
        exec_=exec_,
        wait=wait,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    id: str,
    exec_: str,
    *,
    client: AuthenticatedClient | Client,
    wait: bool | Unset = UNSET,
) -> Any | Error | Exec | None:
    """Read, wait for or attach to an exec"""

    return (
        await asyncio_detailed(
            id=id,
            exec_=exec_,
            client=client,
            wait=wait,
        )
    ).parsed
