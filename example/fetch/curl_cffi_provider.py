"""TLS-impersonating fetcher backed by `curl_cffi`.

A drop-in replacement for the plain httpx provider that mimics a real
Chrome browser at the **TLS / JA3 fingerprint** level via libcurl-impersonate.
A surprising fraction of "Cloudflare blocks" are actually
TLS-fingerprint blocks against the `requests` / `httpx` openssl handshake;
Chrome's BoringSSL handshake gets through them with no extra ops cost.

This provider is the recommended first tier above the plain httpx baseline
because it adds zero infrastructure (no extra container, no headless browser)
yet defeats most TLS-fingerprint-based WAFs in the wild.

The provider self-disables (`is_available()` returns False) when `curl_cffi`
isn't installed, so deployments that don't want the extra dependency can
simply omit it from their environment and the registry will skip past this
tier.
"""

from __future__ import annotations

import asyncio
import logging

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
_DEFAULT_IMPERSONATE = "chrome124"

try:
    from curl_cffi.requests import AsyncSession  # type: ignore[import-not-found]

    _CURL_CFFI_AVAILABLE = True
except ImportError:  # pragma: no cover - optional dependency
    AsyncSession = None  # type: ignore[assignment,misc]
    _CURL_CFFI_AVAILABLE = False


class CurlCffiContentFetcher(ContentFetcherProvider):
    """Async HTTP fetcher backed by `curl_cffi.requests.AsyncSession`."""

    def __init__(
        self,
        timeout: float = _DEFAULT_TIMEOUT,
        max_concurrent: int = _DEFAULT_MAX_CONCURRENT,
        impersonate: str | None = None,
    ) -> None:
        self._timeout = timeout
        self._semaphore = asyncio.Semaphore(max_concurrent)
        self._impersonate = impersonate
        self._session: object | None = None  # AsyncSession when curl_cffi is installed

    @property
    def provider_id(self) -> str:
        return "curl_cffi"

    async def is_available(self) -> bool:
        return _CURL_CFFI_AVAILABLE

    def _get_session(self) -> object:
        if not _CURL_CFFI_AVAILABLE:
            raise RuntimeError("curl_cffi is not installed")
        if self._session is None:
            impersonate = self._impersonate or getattr(
                get_settings(), "fetch_curl_cffi_impersonate", _DEFAULT_IMPERSONATE
            )
            self._session = AsyncSession(impersonate=impersonate)  # type: ignore[misc]
        return self._session

    async def fetch(self, uri: str) -> FetchResult:
        if not _CURL_CFFI_AVAILABLE:
            return FetchResult(uri=uri, error="curl_cffi not installed")

        async with self._semaphore:
            try:
                session = self._get_session()
                response = await session.get(uri, timeout=self._timeout, allow_redirects=True)  # type: ignore[attr-defined]
            except Exception as e:
                logger.debug("curl_cffi error fetching %s: %s", uri, e)
                # curl_cffi raises a wide variety of CurlError subclasses for
                # transport/timeouts; treat any escape as a generic provider
                # failure so the registry can fall through to the next tier.
                return FetchResult(uri=uri, error=str(e))

            status_code = getattr(response, "status_code", 0)
            if status_code >= 400:
                return FetchResult(uri=uri, error=f"HTTP {status_code}")

            headers = getattr(response, "headers", {}) or {}
            content_type = headers.get("content-type", "") or headers.get("Content-Type", "")
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
        if self._session is not None:
            try:
                await self._session.close()  # type: ignore[attr-defined]
            except Exception:
                logger.debug("curl_cffi close failed", exc_info=True)
            self._session = None
