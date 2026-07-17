"""Fallback-chain registry that composes `ContentFetcherProvider` instances.

The registry is the public entry point used by every worker that needs to
fetch a URL.  It owns:

- An ordered list of providers (the *default chain*).
- An optional `HostPreferenceStore` that records "for host X, provider Y
  was the last one to succeed".
- An optional static map of host-to-preferred-provider overrides from
  config (`fetch_host_overrides`).

`fetch(uri)` resolves an effective preferred provider from those three
sources, tries it first, then falls back to the rest of the chain (skipping
the already-tried preferred id) on failure.  Every attempt is recorded on
the returned `FetchResult.attempts` list so the UI and the API can show the
full audit trail to the user.

Mirrors the existing `ProviderRegistry` for `KnowledgeProvider`, but with
the *order* of providers being meaningful — search providers are queried
in parallel and deduplicated, whereas fetchers are tried sequentially with
short-circuit on success.
"""

from __future__ import annotations

import logging
import time
from collections.abc import Awaitable, Callable

from kt_providers.fetch.base import ContentFetcherProvider
from kt_providers.fetch.host_pref import HostPreferenceStore, host_of
from kt_providers.fetch.types import FetchAttempt, FetchResult
from kt_providers.fetch.url_safety import UnsafeUrlError

logger = logging.getLogger(__name__)

#: Type of the SSRF validator hook.  See ``kt_providers.fetch.url_safety``.
UrlValidator = Callable[[str], Awaitable[None]]


class FetchProviderRegistry:
    """Composes multiple fetch providers into a configurable fallback chain."""

    def __init__(
        self,
        providers: list[ContentFetcherProvider],
        chain: list[str],
        *,
        host_overrides: dict[str, str] | None = None,
        host_pref_store: HostPreferenceStore | None = None,
        url_validator: UrlValidator | None = None,
        post_fetch_hooks: list[Callable[[str, FetchResult], Awaitable[FetchResult]]] | None = None,
    ) -> None:
        """
        Args:
            url_validator: Optional async hook called for every URL before
                any provider is invoked.  When None, no validation is done
                — this is the test default; ``build_fetch_registry`` always
                injects the real SSRF guard for production code paths.
                A validator should raise ``UnsafeUrlError`` to reject a URL.
            post_fetch_hooks: Optional list of async callables invoked
                sequentially after a provider succeeds.  Each hook receives
                ``(uri, result)`` and returns a (possibly enriched)
                ``FetchResult``.  Exceptions are caught and logged so a
                failing hook never breaks the fetch.
        """
        self._providers: dict[str, ContentFetcherProvider] = {p.provider_id: p for p in providers}
        self._chain: list[str] = list(chain)
        self._host_overrides: dict[str, str] = {k.lower(): v for k, v in (host_overrides or {}).items()}
        self._host_pref_store = host_pref_store
        self._url_validator = url_validator
        self._post_fetch_hooks: list[Callable[[str, FetchResult], Awaitable[FetchResult]]] = post_fetch_hooks or []

    @property
    def chain(self) -> list[str]:
        """The default fallback chain (immutable view-equivalent copy)."""
        return list(self._chain)

    def has_provider(self, provider_id: str) -> bool:
        return provider_id in self._providers

    def get_provider(self, provider_id: str) -> ContentFetcherProvider | None:
        """Look up a registered provider by id, or ``None`` if not present.

        Public accessor used by tests and any caller that needs to inspect
        provider state (e.g. ``is_public``) without reaching into the private
        ``_providers`` dict.
        """
        return self._providers.get(provider_id)

    async def fetch(
        self,
        uri: str,
        *,
        preferred: str | None = None,
        chain: list[str] | None = None,
    ) -> FetchResult:
        """Fetch `uri` via the configured chain.

        Args:
            uri: The URL to retrieve.
            preferred: Explicit provider id to try first.  Falls back to the
                rest of the chain on failure.  Highest priority of the three
                preferred-resolution sources.
            chain: Override the default chain for this single call (rarely
                needed; mostly for tests and exotic call sites).

        Returns:
            `FetchResult` with `provider_id` and `attempts` populated.  When
            every provider fails, returns a result with `error="all fetchers
            failed"` and the full `attempts` log.

        Raises:
            Nothing.  SSRF rejections are returned as a non-success
            FetchResult with `error="unsafe URL: <reason>"` and a single
            synthetic attempt entry, so callers see them on the same code
            path as ordinary failures.
        """
        # SSRF gate: scheme + DNS-resolved-IP check.  Fails closed and
        # short-circuits the chain so no provider ever sees an unsafe URL.
        if self._url_validator is not None:
            try:
                await self._url_validator(uri)
            except UnsafeUrlError as e:
                logger.warning("rejecting unsafe URL %s: %s", uri, e)
                return FetchResult(
                    uri=uri,
                    error=f"unsafe URL: {e}",
                    attempts=[
                        FetchAttempt(
                            provider_id="url_safety",
                            success=False,
                            error=str(e),
                            elapsed_ms=0,
                        )
                    ],
                )

        host = host_of(uri)
        effective_preferred = await self._resolve_preferred(host, explicit=preferred)
        base_chain = chain if chain is not None else self._chain

        order: list[str] = []
        if effective_preferred:
            order.append(effective_preferred)
        order.extend(pid for pid in base_chain if pid != effective_preferred)

        attempts: list[FetchAttempt] = []
        for pid in order:
            provider = self._providers.get(pid)
            if provider is None:
                attempts.append(FetchAttempt(pid, success=False, error="provider not registered", elapsed_ms=0))
                continue
            try:
                available = await provider.is_available()
            except Exception as e:
                # Log with traceback so the underlying cause is recoverable
                # from the worker logs even though the registry itself
                # swallows the exception to keep the chain marching.
                logger.warning(
                    "fetch provider %s.is_available() raised on %s: %s",
                    pid,
                    uri,
                    e,
                    exc_info=True,
                )
                attempts.append(FetchAttempt(pid, success=False, error=f"is_available raised: {e}", elapsed_ms=0))
                continue
            if not available:
                attempts.append(FetchAttempt(pid, success=False, error="unavailable", elapsed_ms=0))
                continue

            t0 = time.perf_counter()
            try:
                result = await provider.fetch(uri)
            except Exception as e:  # defensive — providers should not raise
                logger.warning("fetch provider %s raised on %s: %s", pid, uri, e, exc_info=True)
                attempts.append(FetchAttempt(pid, success=False, error=f"exception: {e}", elapsed_ms=_ms(t0)))
                continue

            attempts.append(
                FetchAttempt(
                    pid,
                    success=result.success,
                    error=result.error,
                    elapsed_ms=_ms(t0),
                )
            )
            if result.success:
                result.provider_id = pid
                result.is_public = provider.is_public
                result.attempts = attempts
                # Record the winning provider for this host so future fetches
                # can skip earlier (failing) tiers.  Skip recording for the
                # very first link in the default chain — that's the natural
                # default and learning it adds no value.
                if self._host_pref_store is not None and host and base_chain and pid != base_chain[0]:
                    try:
                        await self._host_pref_store.record(host, pid)
                    except Exception:
                        logger.debug("host-pref record failed for %s", host, exc_info=True)

                # Run post-fetch hooks (e.g. DOI enrichment).
                for hook in self._post_fetch_hooks:
                    try:
                        result = await hook(uri, result)
                    except Exception:
                        logger.debug("post-fetch hook failed for %s", uri, exc_info=True)

                return result

        # Every provider failed.  Return a synthetic failure with the audit
        # trail attached so callers can surface it to the user.
        last_error = next((a.error for a in reversed(attempts) if a.error), None)
        if (
            effective_preferred
            and self._host_pref_store is not None
            and host
            and any(a.provider_id == effective_preferred and not a.success for a in attempts)
        ):
            # Learned preference is now stale — drop it so we re-learn next time.
            try:
                await self._host_pref_store.forget(host)
            except Exception:
                logger.debug("host-pref forget failed for %s", host, exc_info=True)
        return FetchResult(
            uri=uri,
            error=last_error or "all fetchers failed",
            attempts=attempts,
        )

    async def _resolve_preferred(self, host: str | None, *, explicit: str | None) -> str | None:
        """Resolve the effective preferred provider id.

        Priority:
            1. explicit caller arg
            2. learned per-host preference (Redis)
            3. static `fetch_host_overrides` config

        Returns None when none of the above produce a registered provider id.
        """
        if explicit and self.has_provider(explicit):
            return explicit
        if not host:
            return None
        if self._host_pref_store is not None:
            try:
                learned = await self._host_pref_store.get(host)
            except Exception:
                logger.debug("host-pref get failed for %s", host, exc_info=True)
                learned = None
            if learned and self.has_provider(learned):
                return learned
        # Static overrides — match exact host first, then suffix-match (so
        # "cell.com" in config matches "www.cell.com").
        override = self._host_overrides.get(host)
        if override is None:
            for key, value in self._host_overrides.items():
                if host == key or host.endswith("." + key):
                    override = value
                    break
        if override and self.has_provider(override):
            return override
        return None

    async def fetch_many(
        self,
        uris: list[str],
        *,
        chain: list[str] | None = None,
    ) -> list[FetchResult]:
        """Convenience: fetch multiple URIs concurrently.

        Each URI goes through its own provider chain (so different hosts can
        end up using different providers in a single batch).

        ``chain`` — when given — overrides the default chain for every URI
        in this batch. Used by worker tasks to honour a graph-type's
        :attr:`GraphTypeComposition.fetch_chain` without reconstructing a
        per-graph registry.
        """
        import asyncio

        tasks = [self.fetch(u, chain=chain) for u in uris]
        return list(await asyncio.gather(*tasks))

    async def close(self) -> None:
        """Close every registered provider and closeable post-fetch hooks."""
        for provider in self._providers.values():
            try:
                await provider.close()
            except Exception:
                logger.debug("provider %s close failed", provider.provider_id, exc_info=True)
        for hook in self._post_fetch_hooks:
            close_fn = getattr(hook, "close", None) or getattr(getattr(hook, "__self__", None), "close", None)
            if close_fn is not None:
                try:
                    await close_fn()
                except Exception:
                    logger.debug("post-fetch hook close failed", exc_info=True)


def _ms(t0: float) -> int:
    return int((time.perf_counter() - t0) * 1000)
