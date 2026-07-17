"""Post-fetch DOI enrichment via Crossref and Unpaywall.

This module is the **resolution layer** for academic content.  It runs
*after* a successful page fetch (curl_cffi, httpx, flaresolverr, etc.)
and checks whether the fetched page belongs to a known academic publisher
and contains a DOI.  When a DOI is found it:

1. Queries Crossref for canonical scholarly metadata (title, authors,
   abstract, journal, publication date).
2. Queries Unpaywall for an open-access PDF link.
3. If an OA PDF exists, downloads it and upgrades the ``FetchResult``
   content with the full text.
4. Merges Crossref metadata fields into ``html_metadata`` so downstream
   consumers (multigraph cache, ingest pipeline) can use them.

The enricher is wired into ``FetchProviderRegistry`` as a post-fetch hook
by ``build_fetch_registry``.  It is **not** a ``ContentFetcherProvider`` --
it enhances results rather than competing in the fetch chain.
"""

from __future__ import annotations

import logging
import re
import time
from urllib.parse import urlparse

import httpx

from kt_config.settings import get_settings
from kt_providers.fetch.canonical import _doi_from_url
from kt_providers.fetch.extract import classify_content_type, extract_pdf
from kt_providers.fetch.types import FetchAttempt, FetchResult
from kt_providers.fetch.url_safety import UnsafeUrlError, validate_fetch_url

logger = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# Known academic publisher hosts where DOI enrichment is worthwhile.
# Anything else is skipped so we don't waste a Crossref roundtrip on a
# random blog post.
# ---------------------------------------------------------------------------
PUBLISHER_HOSTS: frozenset[str] = frozenset(
    {
        "cell.com",
        "www.cell.com",
        "sciencedirect.com",
        "www.sciencedirect.com",
        "linkinghub.elsevier.com",
        "nature.com",
        "www.nature.com",
        "link.springer.com",
        "springer.com",
        "onlinelibrary.wiley.com",
        "wiley.com",
        "tandfonline.com",
        "www.tandfonline.com",
        "journals.sagepub.com",
        "academic.oup.com",
        "ieeexplore.ieee.org",
        "dl.acm.org",
        "jstor.org",
        "www.jstor.org",
        "pubmed.ncbi.nlm.nih.gov",
        "www.ncbi.nlm.nih.gov",
        "pmc.ncbi.nlm.nih.gov",
        "biorxiv.org",
        "www.biorxiv.org",
        "medrxiv.org",
        "www.medrxiv.org",
    }
)

CROSSREF_API = "https://api.crossref.org/works/{doi}"
UNPAYWALL_API = "https://api.unpaywall.org/v2/{doi}"


class DoiEnricher:
    """Post-fetch DOI enrichment via Crossref and Unpaywall.

    This class is used as a post-fetch hook on :class:`FetchProviderRegistry`.
    Its :meth:`enrich` method signature matches the hook protocol::

        async def enrich(uri: str, result: FetchResult) -> FetchResult
    """

    def __init__(self, timeout: float = 10.0) -> None:
        self._timeout = timeout
        self._client: httpx.AsyncClient | None = None

    async def _client_(self) -> httpx.AsyncClient:
        if self._client is None or self._client.is_closed:
            ua = get_settings().fetch_user_agent
            self._client = httpx.AsyncClient(
                timeout=httpx.Timeout(self._timeout),
                follow_redirects=True,
                headers={"User-Agent": ua, "Accept": "application/json"},
            )
        return self._client

    # ------------------------------------------------------------------
    # Public API
    # ------------------------------------------------------------------

    async def enrich(self, uri: str, result: FetchResult) -> FetchResult:
        """Attempt DOI-based enrichment on a successful ``FetchResult``.

        Returns the original *result* unmodified when:

        - The host is not a known publisher.
        - No DOI can be extracted from ``html_metadata`` or the URL.
        - Crossref lookup fails or returns sparse metadata.

        Otherwise merges Crossref metadata into ``html_metadata`` and
        optionally upgrades ``content`` with OA PDF full text.
        """
        host = urlparse(uri).netloc.lower()
        if host not in PUBLISHER_HOSTS:
            return result

        doi = self._extract_doi(uri, result)
        if doi is None:
            return result

        t0 = time.perf_counter()

        try:
            metadata = await self._fetch_crossref(doi)
        except Exception as e:
            logger.debug("DOI enrichment: Crossref failed for %s: %s", doi, e)
            result.attempts.append(
                FetchAttempt("doi_enricher", success=False, error=f"crossref: {e}", elapsed_ms=_ms(t0))
            )
            return result

        if metadata is None:
            result.attempts.append(
                FetchAttempt("doi_enricher", success=False, error=f"DOI {doi} not in Crossref", elapsed_ms=_ms(t0))
            )
            return result

        # Merge Crossref metadata into html_metadata.
        meta = dict(result.html_metadata or {})
        meta["doi"] = doi
        title_value = metadata.get("title")
        if isinstance(title_value, list) and title_value:
            meta["title"] = str(title_value[0])
        elif isinstance(title_value, str):
            meta["title"] = title_value
        publisher = metadata.get("publisher")
        if isinstance(publisher, str):
            meta["publisher"] = publisher
        meta["enriched_by"] = "crossref"

        # Best-effort Unpaywall OA PDF lookup.
        oa_url: str | None = None
        try:
            oa_url = await self._fetch_unpaywall_oa(doi)
        except Exception:
            logger.debug("DOI enrichment: Unpaywall failed for %s", doi, exc_info=True)

        meta["oa_pdf_url"] = oa_url
        result.html_metadata = meta

        # If we have an OA PDF, try to upgrade the content with full text.
        if oa_url:
            crossref_body = format_metadata(metadata)
            upgraded = await self._try_fetch_pdf(uri, oa_url, crossref_body, meta)
            if upgraded is not None:
                # Preserve the original provider_id and attempts — just
                # upgrade content, content_type, and PDF metadata.
                result.content = upgraded.content
                result.content_type = upgraded.content_type
                result.page_count = upgraded.page_count
                result.pdf_metadata = upgraded.pdf_metadata
                result.html_metadata = upgraded.html_metadata

        result.attempts.append(FetchAttempt("doi_enricher", success=True, error=None, elapsed_ms=_ms(t0)))
        return result

    # ------------------------------------------------------------------
    # DOI extraction
    # ------------------------------------------------------------------

    @staticmethod
    def _extract_doi(uri: str, result: FetchResult) -> str | None:
        """Extract DOI from the fetch result metadata or the URL."""
        if result.html_metadata:
            meta_doi = result.html_metadata.get("doi")
            if meta_doi and isinstance(meta_doi, str) and meta_doi.strip():
                return meta_doi.strip()
        return _doi_from_url(uri)

    # ------------------------------------------------------------------
    # Crossref / Unpaywall
    # ------------------------------------------------------------------

    async def _fetch_crossref(self, doi: str) -> dict[str, object] | None:
        client = await self._client_()
        settings = get_settings()
        headers = {}
        email = getattr(settings, "crossref_email", None)
        if email:
            headers["User-Agent"] = f"{settings.fetch_user_agent} (mailto:{email})"
        response = await client.get(CROSSREF_API.format(doi=doi), headers=headers or None)
        if response.status_code == 404:
            return None
        response.raise_for_status()
        data = response.json()
        message = data.get("message")
        return message if isinstance(message, dict) else None

    async def _fetch_unpaywall_oa(self, doi: str) -> str | None:
        settings = get_settings()
        email = getattr(settings, "unpaywall_email", None) or getattr(settings, "crossref_email", None)
        if not email:
            return None
        client = await self._client_()
        response = await client.get(UNPAYWALL_API.format(doi=doi), params={"email": email})
        if response.status_code != 200:
            return None
        data = response.json()
        best = data.get("best_oa_location") or {}
        url = best.get("url_for_pdf") or best.get("url")
        if not url:
            return None
        url_str = str(url)
        try:
            await validate_fetch_url(url_str)
        except UnsafeUrlError as e:
            logger.warning(
                "rejecting unsafe Unpaywall PDF url for DOI %s: %s (%s)",
                doi,
                url_str,
                e,
            )
            return None
        return url_str

    async def _try_fetch_pdf(
        self,
        uri: str,
        oa_url: str,
        metadata_body: str,
        meta_out: dict[str, str | None],
    ) -> FetchResult | None:
        """Download an OA PDF and extract full text, returning None on failure."""
        try:
            client = await self._client_()
            response = await client.get(oa_url, headers={"Accept": "application/pdf, */*"})
        except Exception:
            logger.debug("OA PDF download failed for %s", oa_url, exc_info=True)
            return None

        if response.status_code >= 400:
            logger.debug("OA PDF returned %s for %s", response.status_code, oa_url)
            return None

        ct = response.headers.get("content-type", "")
        if classify_content_type(ct) != "pdf":
            logger.debug("OA URL returned non-PDF content-type %s for %s", ct, oa_url)
            return None

        pdf_result = extract_pdf(uri, response.content, ct)
        if not pdf_result.success:
            logger.debug("PDF extraction failed for %s: %s", oa_url, pdf_result.error)
            return None

        combined = f"{metadata_body}\n\n---\n\n{pdf_result.content}"
        return FetchResult(
            uri=uri,
            content=combined,
            content_type="application/pdf",
            page_count=pdf_result.page_count,
            pdf_metadata=pdf_result.pdf_metadata,
            html_metadata=meta_out,
        )

    # ------------------------------------------------------------------
    # Lifecycle
    # ------------------------------------------------------------------

    async def close(self) -> None:
        if self._client is not None and not self._client.is_closed:
            await self._client.aclose()
            self._client = None


# ------------------------------------------------------------------
# Shared utilities
# ------------------------------------------------------------------


def format_metadata(message: dict[str, object]) -> str:
    """Render a Crossref ``message`` payload as a plain-text body."""
    parts: list[str] = []

    title = message.get("title")
    if isinstance(title, list) and title:
        parts.append(f"Title: {title[0]}")
    elif isinstance(title, str):
        parts.append(f"Title: {title}")

    authors = message.get("author")
    if isinstance(authors, list) and authors:
        names = [
            " ".join(filter(None, [a.get("given"), a.get("family")]))  # type: ignore[union-attr]
            for a in authors
            if isinstance(a, dict)
        ]
        names = [n for n in names if n]
        if names:
            parts.append(f"Authors: {', '.join(names)}")

    pub = message.get("publisher")
    if pub:
        parts.append(f"Publisher: {pub}")

    container = message.get("container-title")
    if isinstance(container, list) and container:
        parts.append(f"Journal: {container[0]}")

    issued = message.get("issued") or {}
    date_parts = (issued.get("date-parts") or [[]])[0] if isinstance(issued, dict) else []
    if date_parts:
        parts.append(f"Published: {'-'.join(str(p) for p in date_parts)}")

    abstract = message.get("abstract")
    if isinstance(abstract, str) and abstract:
        clean = re.sub(r"<[^>]+>", "", abstract).strip()
        if clean:
            parts.append(f"\nAbstract:\n{clean}")

    doi = message.get("DOI")
    if doi:
        parts.append(f"\nDOI: {doi}")

    return "\n".join(parts)


def _ms(t0: float) -> int:
    return int((time.perf_counter() - t0) * 1000)
