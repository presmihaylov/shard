from http import HTTPStatus
from typing import Any, cast
from urllib.parse import quote

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.create_request import CreateRequest
from ...models.error import Error
from ...models.sandbox import Sandbox
from ...types import UNSET, Response, Unset


def _get_kwargs(
    *,
    body: CreateRequest | Unset = UNSET,
    wait: bool | Unset = UNSET,
) -> dict[str, Any]:
    headers: dict[str, Any] = {}

    params: dict[str, Any] = {}

    params["wait"] = wait

    params = {k: v for k, v in params.items() if v is not UNSET and v is not None}

    _kwargs: dict[str, Any] = {
        "method": "post",
        "url": "/v0/sandboxes",
        "params": params,
    }

    if not isinstance(body, Unset):
        _kwargs["json"] = body.to_dict()

    headers["Content-Type"] = "application/json"

    _kwargs["headers"] = headers
    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Error | Sandbox:
    if response.status_code == 201:
        response_201 = Sandbox.from_dict(response.json())

        return response_201

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
    *,
    client: AuthenticatedClient | Client,
    body: CreateRequest | Unset = UNSET,
    wait: bool | Unset = UNSET,
) -> Response[Error | Sandbox]:
    """create a sandbox"""

    kwargs = _get_kwargs(
        body=body,
        wait=wait,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    *,
    client: AuthenticatedClient | Client,
    body: CreateRequest | Unset = UNSET,
    wait: bool | Unset = UNSET,
) -> Error | Sandbox | None:
    """create a sandbox"""

    return sync_detailed(
        client=client,
        body=body,
        wait=wait,
    ).parsed


async def asyncio_detailed(
    *,
    client: AuthenticatedClient | Client,
    body: CreateRequest | Unset = UNSET,
    wait: bool | Unset = UNSET,
) -> Response[Error | Sandbox]:
    """create a sandbox"""

    kwargs = _get_kwargs(
        body=body,
        wait=wait,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    *,
    client: AuthenticatedClient | Client,
    body: CreateRequest | Unset = UNSET,
    wait: bool | Unset = UNSET,
) -> Error | Sandbox | None:
    """create a sandbox"""

    return (
        await asyncio_detailed(
            client=client,
            body=body,
            wait=wait,
        )
    ).parsed
