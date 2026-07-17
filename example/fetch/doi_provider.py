"""DOI-direct provider for ``doi.org`` / ``dx.doi.org`` URLs.

When a user submits a ``doi.org`` link the DOI is embedded in the URL path,
so we can go straight to Crossref/Unpaywall without fetching a landing page.
This provider handles **only** those two hosts; all other academic publisher
URLs are fetched by the normal chain (curl_cffi, httpx, flaresolverr) and
then enriched by the :class:`~kt_providers.fetch.doi_enricher.DoiEnricher`
post-fetch hook.
"""

from __future__ import annotations

import logging
from urllib.parse import urlparse

from kt_providers.fetch.base import ContentFetcherProvider
from kt_providers.fetch.doi_enricher import DoiEnricher, format_metadata
from kt_providers.fetch.types import MIN_EXTRACTED_LENGTH, FetchResult

logger = logging.getLogger(__name__)

_DOI_ORG_HOSTS = frozenset({"doi.org", "dx.doi.org", "www.doi.org"})


class DoiContentFetcher(ContentFetcherProvider):
    """Fetcher that resolves ``doi.org`` URLs via Crossref/Unpaywall."""

    def __init__(self, timeout: float = 10.0) -> None:
        self._enricher = DoiEnricher(timeout=timeout)

    @property
    def provider_id(self) -> str:
        return "doi"

    async def is_available(self) -> bool:
        return True

    async def fetch(self, uri: str) -> FetchResult:
        host = urlparse(uri).netloc.lower()
        if host not in _DOI_ORG_HOSTS:
            return FetchResult(uri=uri, error="not a doi.org URL")

        doi = urlparse(uri).path.lstrip("/")
        if not doi:
            return FetchResult(uri=uri, error="empty DOI path")

        try:
            metadata = await self._enricher._fetch_crossref(doi)
        except Exception as e:
            logger.debug("Crossref fetch failed for %s: %s", doi, e)
            return FetchResult(uri=uri, error=f"crossref error: {e}")

        if metadata is None:
            return FetchResult(uri=uri, error=f"DOI {doi} not in Crossref")

        body = format_metadata(metadata)
        if not body or len(body) < MIN_EXTRACTED_LENGTH:
            return FetchResult(
                uri=uri,
                error="Crossref metadata too sparse",
                content_type="application/json",
            )

        # Best-effort: try to enrich with full-text from an Unpaywall OA PDF.
        oa_url = None
        try:
            oa_url = await self._enricher._fetch_unpaywall_oa(doi)
        except Exception:
            logger.debug("Unpaywall lookup failed for %s", doi, exc_info=True)

        meta_out: dict[str, str | None] = {"doi": doi, "oa_pdf_url": oa_url}
        title_value = metadata.get("title")
        if isinstance(title_value, list) and title_value:
            meta_out["title"] = str(title_value[0])
        elif isinstance(title_value, str):
            meta_out["title"] = title_value
        publisher = metadata.get("publisher")
        if isinstance(publisher, str):
            meta_out["publisher"] = publisher

        # If we have an OA PDF URL, download and extract full text.
        if oa_url:
            pdf_result = await self._enricher._try_fetch_pdf(uri, oa_url, body, meta_out)
            if pdf_result is not None:
                return pdf_result

        return FetchResult(
            uri=uri,
            content=body,
            content_type="text/plain",
            html_metadata=meta_out,
        )

    async def close(self) -> None:
        await self._enricher.close()
