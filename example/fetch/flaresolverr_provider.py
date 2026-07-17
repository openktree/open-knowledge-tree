"""FlareSolverr / Byparr fetcher (heaviest in-fallback tier).

[FlareSolverr](https://github.com/FlareSolverr/FlareSolverr) and its more
actively maintained fork [Byparr](https://github.com/ThePhaseless/Byparr) run
a real headless undetected-Chromium behind a small HTTP API.  Sites that
detect TLS impersonation (curl_cffi) or run a JavaScript challenge are
typically still solvable by Byparr because it's an actual browser.

This is the heaviest tier we ship out of the box — but a single shared
container handles every blocked request for the whole cluster, so the cost
is bounded.

The provider speaks the FlareSolverr v1 protocol:
    POST $URL  body={"cmd": "request.get", "url": ..., "maxTimeout": ms}
    response   {"status": "ok", "solution": {"response": "<html>...", ...}}

The provider self-disables when the `fetch_flaresolverr_url` setting is
empty so dev environments without the container fall through cleanly to
the next tier.

This provider is shipped in PR 1 *as a stub*: it's wired into the registry
and self-disables until the setting is populated.  PR 2 deploys the actual
Byparr container in compose + k8s and turns this on for the dev cluster.
"""

from __future__ import annotations

import logging

import httpx

from kt_config.settings import get_settings
from kt_providers.fetch.base import ContentFetcherProvider
from kt_providers.fetch.extract import (
    classify_content_type,
    extract_html,
    extract_text,
)
from kt_providers.fetch.types import FetchResult

logger = logging.getLogger(__name__)


class FlareSolverrContentFetcher(ContentFetcherProvider):
    """Fetcher backed by a FlareSolverr/Byparr container."""

    def __init__(self, endpoint: str | None = None, timeout: float = 60.0) -> None:
        self._endpoint = endpoint
        self._timeout = timeout
        self._client: httpx.AsyncClient | None = None

    @property
    def provider_id(self) -> str:
        return "flaresolverr"

    def _resolved_endpoint(self) -> str | None:
        return self._endpoint or getattr(get_settings(), "fetch_flaresolverr_url", None)

    async def is_available(self) -> bool:
        return bool(self._resolved_endpoint())

    async def _client_(self) -> httpx.AsyncClient:
        if self._client is None or self._client.is_closed:
            self._client = httpx.AsyncClient(timeout=httpx.Timeout(self._timeout))
        return self._client

    async def fetch(self, uri: str) -> FetchResult:
        endpoint = self._resolved_endpoint()
        if not endpoint:
            return FetchResult(uri=uri, error="flaresolverr endpoint not configured")

        payload = {
            "cmd": "request.get",
            "url": uri,
            "maxTimeout": int(self._timeout * 1000),
        }
        try:
            client = await self._client_()
            response = await client.post(endpoint, json=payload)
            response.raise_for_status()
            data = response.json()
        except httpx.TimeoutException:
            return FetchResult(uri=uri, error="flaresolverr: timeout")
        except httpx.HTTPStatusError as e:
            return FetchResult(uri=uri, error=f"flaresolverr: HTTP {e.response.status_code}")
        except Exception as e:
            logger.debug("flaresolverr error fetching %s: %s", uri, e)
            return FetchResult(uri=uri, error=f"flaresolverr: {e}")

        status = data.get("status")
        if status != "ok":
            return FetchResult(uri=uri, error=f"flaresolverr: {data.get('message', 'unknown error')}")

        solution = data.get("solution") or {}
        html = solution.get("response", "") or ""
        headers = solution.get("headers") or {}
        content_type = headers.get("content-type") or headers.get("Content-Type") or "text/html"
        classified = classify_content_type(content_type)

        # Byparr/FlareSolverr decode bytes to text for us — they cannot deliver
        # raw PDF/image bytes.  PDFs and images therefore can't be handled
        # via this provider; the registry should fall through to a tier that
        # can fetch raw bytes when those content types are encountered.
        if classified in ("pdf", "image"):
            return FetchResult(
                uri=uri,
                error=f"flaresolverr: cannot return raw {classified} bytes",
                content_type=content_type,
            )

        if "html" in content_type.lower():
            return extract_html(uri, html, content_type)
        return extract_text(uri, html, content_type)

    async def close(self) -> None:
        if self._client is not None and not self._client.is_closed:
            await self._client.aclose()
            self._client = None
