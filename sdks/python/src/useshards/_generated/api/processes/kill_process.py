from http import HTTPStatus
from typing import Any, cast
from urllib.parse import quote

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.error import Error
from ...models.process import Process
from ...models.process_kill_request import ProcessKillRequest
from ...types import UNSET, Response, Unset


def _get_kwargs(
    id: str,
    name: str,
    *,
    body: ProcessKillRequest | Unset = UNSET,
) -> dict[str, Any]:
    headers: dict[str, Any] = {}

    _kwargs: dict[str, Any] = {
        "method": "post",
        "url": "/v0/sandboxes/{id}/processes/{name}/kill".format(
            id=quote(str(id), safe=""),
            name=quote(str(name), safe=""),
        ),
    }

    if not isinstance(body, Unset):
        _kwargs["json"] = body.to_dict()

    headers["Content-Type"] = "application/json"

    _kwargs["headers"] = headers
    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Error | Process:
    if response.status_code == 200:
        response_200 = Process.from_dict(response.json())

        return response_200

    response_default = Error.from_dict(response.json())

    return response_default


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[Error | Process]:
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
    body: ProcessKillRequest | Unset = UNSET,
) -> Response[Error | Process]:
    """End a process and cancel its restarts"""

    kwargs = _get_kwargs(
        id=id,
        name=name,
        body=body,
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
    body: ProcessKillRequest | Unset = UNSET,
) -> Error | Process | None:
    """End a process and cancel its restarts"""

    return sync_detailed(
        id=id,
        name=name,
        client=client,
        body=body,
    ).parsed


async def asyncio_detailed(
    id: str,
    name: str,
    *,
    client: AuthenticatedClient | Client,
    body: ProcessKillRequest | Unset = UNSET,
) -> Response[Error | Process]:
    """End a process and cancel its restarts"""

    kwargs = _get_kwargs(
        id=id,
        name=name,
        body=body,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    id: str,
    name: str,
    *,
    client: AuthenticatedClient | Client,
    body: ProcessKillRequest | Unset = UNSET,
) -> Error | Process | None:
    """End a process and cancel its restarts"""

    return (
        await asyncio_detailed(
            id=id,
            name=name,
            client=client,
            body=body,
        )
    ).parsed
