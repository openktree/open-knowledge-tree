"""Plain `httpx`-based content fetcher.

The cheapest possible tier — a stock async HTTP client with a configurable
User-Agent and a small set of browser-like headers.  No TLS impersonation,
no JS rendering.  Used as the default first-tier strategy for sites with no
anti-bot protection.

Sites with even mild WAFs (Cloudflare, Datadome, etc.) will typically need
the `curl_cffi_provider` (TLS fingerprint impersonation) or the
`flaresolverr_provider` (real headless Chromium) further down the chain.
"""

from __future__ import annotations

import asyncio
import logging

import httpx

from kt_config.settings import get_settings
from kt_providers.fetch.base import ContentFetcherProvider
from kt_providers.fetch.extract import (
    classify_content_type,
    extract_html,
    extract_image,
    extract_pdf,
    extract_text,
)
from kt_providers.fetch.types import FetchResult

logger = logging.getLogger(__name__)

_DEFAULT_TIMEOUT = 15.0
_DEFAULT_MAX_CONCURRENT = 3


def _browser_headers(user_agent: str) -> dict[str, str]:
    """Headers that mimic a real browser request.

    A bare User-Agent is often enough to get past naive UA-only checks but
    fails on WAFs that look at the rest of the header set.  Sending the
    full Accept/Sec-Fetch suite costs nothing and unblocks more sites.
    """
    return {
        "User-Agent": user_agent,
        "Accept": ("text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8"),
        "Accept-Language": "en-US,en;q=0.9",
        "Accept-Encoding": "gzip, deflate, br",
        "Sec-Fetch-Dest": "document",
        "Sec-Fetch-Mode": "navigate",
        "Sec-Fetch-Site": "none",
        "Sec-Fetch-User": "?1",
        "Upgrade-Insecure-Requests": "1",
    }


class HttpxContentFetcher(ContentFetcherProvider):
    """Async HTTP fetcher backed by `httpx.AsyncClient`."""

    def __init__(
        self,
        timeout: float = _DEFAULT_TIMEOUT,
        max_concurrent: int = _DEFAULT_MAX_CONCURRENT,
        user_agent: str | None = None,
    ) -> None:
        self._timeout = timeout
        self._semaphore = asyncio.Semaphore(max_concurrent)
        self._user_agent = user_agent
        self._client: httpx.AsyncClient | None = None

    @property
    def provider_id(self) -> str:
        return "httpx"

    async def is_available(self) -> bool:
        # httpx is a hard dependency of kt-providers; always available.
        return True

    async def _get_client(self) -> httpx.AsyncClient:
        if self._client is None or self._client.is_closed:
            ua = self._user_agent or get_settings().fetch_user_agent
            self._client = httpx.AsyncClient(
                timeout=httpx.Timeout(self._timeout),
                follow_redirects=True,
                headers=_browser_headers(ua),
            )
        return self._client

    async def fetch(self, uri: str) -> FetchResult:
        async with self._semaphore:
            try:
                client = await self._get_client()
                response = await client.get(uri)
                response.raise_for_status()
            except httpx.TimeoutException:
                return FetchResult(uri=uri, error="Timeout")
            except httpx.HTTPStatusError as e:
                return FetchResult(uri=uri, error=f"HTTP {e.response.status_code}")
            except Exception as e:
                logger.debug("Error fetching %s: %s", uri, e)
                return FetchResult(uri=uri, error=str(e))

            content_type = response.headers.get("content-type", "")
            classified = classify_content_type(content_type)

            if classified == "pdf":
                return extract_pdf(uri, response.content, content_type)
            if classified == "image":
                return extract_image(uri, response.content, content_type)
            if classified == "text":
                if "html" in content_type.lower():
                    return extract_html(uri, response.text, content_type)
                return extract_text(uri, response.text, content_type)
            return FetchResult(
                uri=uri,
                error=f"Non-text content type: {content_type}",
                content_type=content_type,
            )

    async def close(self) -> None:
        if self._client is not None and not self._client.is_closed:
            await self._client.aclose()
            self._client = None
