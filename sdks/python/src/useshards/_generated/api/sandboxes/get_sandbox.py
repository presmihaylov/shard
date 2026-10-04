from http import HTTPStatus
from typing import Any, cast
from urllib.parse import quote

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.error import Error
from ...models.inspection import Inspection
from ...types import UNSET, Response, Unset


def _get_kwargs(
    id: str,
    *,
    wait: bool | Unset = UNSET,
) -> dict[str, Any]:
    params: dict[str, Any] = {}

    params["wait"] = wait

    params = {k: v for k, v in params.items() if v is not UNSET and v is not None}

    _kwargs: dict[str, Any] = {
        "method": "get",
        "url": "/v0/sandboxes/{id}".format(
            id=quote(str(id), safe=""),
        ),
        "params": params,
    }

    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Error | Inspection:
    if response.status_code == 200:
        response_200 = Inspection.from_dict(response.json())

        return response_200

    response_default = Error.from_dict(response.json())

    return response_default


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[Error | Inspection]:
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
    wait: bool | Unset = UNSET,
) -> Response[Error | Inspection]:
    """Read a sandbox and the egress rules the host enforces for it"""

    kwargs = _get_kwargs(
        id=id,
        wait=wait,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    id: str,
    *,
    client: AuthenticatedClient | Client,
    wait: bool | Unset = UNSET,
) -> Error | Inspection | None:
    """Read a sandbox and the egress rules the host enforces for it"""

    return sync_detailed(
        id=id,
        client=client,
        wait=wait,
    ).parsed


async def asyncio_detailed(
    id: str,
    *,
    client: AuthenticatedClient | Client,
    wait: bool | Unset = UNSET,
) -> Response[Error | Inspection]:
    """Read a sandbox and the egress rules the host enforces for it"""

    kwargs = _get_kwargs(
        id=id,
        wait=wait,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    id: str,
    *,
    client: AuthenticatedClient | Client,
    wait: bool | Unset = UNSET,
) -> Error | Inspection | None:
    """Read a sandbox and the egress rules the host enforces for it"""

    return (
        await asyncio_detailed(
            id=id,
            client=client,
            wait=wait,
        )
    ).parsed
