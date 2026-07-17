"""Build a `FetchProviderRegistry` from `kt_config.Settings`.

Centralised so workers, the API, and tests don't all need to know how the
provider chain is wired together.  Pass `redis=None` to get a registry
without a learned-host-preference store; pass an `aioredis.Redis` instance
to enable per-host learning.
"""

from __future__ import annotations

import logging
from typing import TYPE_CHECKING

from kt_config.settings import Settings
from kt_flags import get_flag_client
from kt_flags.registry import FLAG_REGISTRY
from kt_providers.fetch.base import ContentFetcherProvider
from kt_providers.fetch.host_pref import (
    HostPreferenceStore,
    InMemoryHostPreferenceStore,
    RedisHostPreferenceStore,
)
from kt_providers.fetch.registry import FetchProviderRegistry
from kt_providers.fetch.url_safety import validate_fetch_url

if TYPE_CHECKING:
    pass

logger = logging.getLogger(__name__)


def build_fetch_registry(
    settings: Settings,
    redis: object | None = None,
    *,
    in_memory_prefs: bool = False,
) -> FetchProviderRegistry:
    """Construct a fully configured `FetchProviderRegistry`.

    Args:
        settings: Loaded application settings.
        redis: Optional async Redis client used for the learned host
            preference store.  When None and `in_memory_prefs` is False, the
            registry runs without a preference store (still respects static
            host overrides and explicit `preferred=` calls).
        in_memory_prefs: When True (mostly for tests), use an in-memory
            preference store instead of Redis.

    Returns:
        A `FetchProviderRegistry` containing one instance of every fetch
        provider whose runtime requirements are satisfied.  Providers
        whose required config is missing are still registered (so the chain
        can be reconfigured at runtime) — they self-disable via
        `is_available()`.
    """
    providers: list[ContentFetcherProvider] = []

    # DOI shortcut — always available.
    from kt_providers.fetch.doi_provider import DoiContentFetcher

    providers.append(DoiContentFetcher(timeout=settings.full_text_fetch_timeout))

    # Plain httpx — always available.
    from kt_providers.fetch.httpx_provider import HttpxContentFetcher

    providers.append(
        HttpxContentFetcher(
            timeout=settings.full_text_fetch_timeout,
            max_concurrent=settings.full_text_fetch_max_urls,
            user_agent=settings.fetch_user_agent,
        )
    )

    # curl_cffi — TLS impersonation; only registers if the package imported.
    try:
        from kt_providers.fetch.curl_cffi_provider import (
            _CURL_CFFI_AVAILABLE,
            CurlCffiContentFetcher,
        )

        if _CURL_CFFI_AVAILABLE:
            providers.append(
                CurlCffiContentFetcher(
                    timeout=settings.full_text_fetch_timeout,
                    max_concurrent=settings.full_text_fetch_max_urls,
                    impersonate=getattr(settings, "fetch_curl_cffi_impersonate", None),
                )
            )
    except Exception:
        logger.debug("curl_cffi provider unavailable", exc_info=True)

    # FlareSolverr — only useful when its endpoint is configured.
    from kt_providers.fetch.flaresolverr_provider import FlareSolverrContentFetcher

    providers.append(
        FlareSolverrContentFetcher(
            endpoint=getattr(settings, "fetch_flaresolverr_url", None),
            timeout=getattr(settings, "fetch_flaresolverr_timeout", 60.0),
        )
    )

    chain_str = getattr(settings, "fetch_provider_chain", "doi,curl_cffi,httpx,flaresolverr")
    chain = [c.strip() for c in chain_str.split(",") if c.strip()]

    host_overrides = getattr(settings, "fetch_host_overrides", None) or {}

    # Apply per-provider is_public overrides from Settings.  Patches the
    # *instance* (not the class) so test doubles and other process-local
    # registries are unaffected.  Future tenant fetchers (jira/sharepoint)
    # declare ``is_public = False`` on the class itself; this hook is for
    # operators who want to flip a normally-public fetcher private when it
    # is pointed at an intranet.
    public_overrides = getattr(settings, "fetch_provider_public_overrides", None) or {}
    for provider in providers:
        if provider.provider_id in public_overrides:
            provider.is_public = public_overrides[provider.provider_id]

    pref_store: HostPreferenceStore | None
    if in_memory_prefs:
        pref_store = InMemoryHostPreferenceStore()
    elif redis is not None:
        pref_store = RedisHostPreferenceStore(
            redis,
            ttl_seconds=getattr(settings, "fetch_host_pref_ttl_seconds", 60 * 60 * 24 * 30),
        )
    else:
        pref_store = None

    # Post-fetch DOI enrichment — queries Crossref/Unpaywall after a
    # provider successfully fetches a page from a known publisher host.
    from collections.abc import Awaitable, Callable

    from kt_providers.fetch.types import FetchResult

    post_fetch_hooks: list[Callable[[str, FetchResult], Awaitable[FetchResult]]] = []
    if getattr(settings, "fetch_doi_enrichment", True):
        from kt_providers.fetch.doi_enricher import DoiEnricher

        enricher = DoiEnricher(timeout=settings.full_text_fetch_timeout)
        post_fetch_hooks.append(enricher.enrich)

    return FetchProviderRegistry(
        providers=providers,
        chain=chain,
        host_overrides=host_overrides,
        host_pref_store=pref_store,
        url_validator=validate_fetch_url,
        post_fetch_hooks=post_fetch_hooks,
    )


def maybe_build_fetch_registry(
    settings: Settings,
    redis: object | None = None,
    *,
    in_memory_prefs: bool = False,
) -> FetchProviderRegistry | None:
    """Return a registry when ``feature.full_text_fetch`` is on, ``None`` otherwise.

    Centralises the flag-gate so the API dependency and every worker
    lifespan read the flag the same way, with the same default, and never
    drift. The default is sourced from the ``kt_flags`` registry so future
    changes to the canonical default don't have to ripple through call
    sites.
    """
    default = bool(FLAG_REGISTRY["feature.full_text_fetch"].default)
    if not get_flag_client().get_boolean("feature.full_text_fetch", default=default):
        return None
    return build_fetch_registry(settings, redis, in_memory_prefs=in_memory_prefs)
