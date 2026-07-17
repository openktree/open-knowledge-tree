"""Per-host learned-preference cache for the fetch provider chain.

When a non-default fetch provider succeeds for a given host, the registry
records that fact here.  Next time we see the same host, the registry tries
the recorded provider *first* (skipping the rest of the chain on success)
instead of marching through every tier from the top.

This is just an optimisation — failures fall back to the full chain — and
the data is intentionally persisted in the same Redis instance the rest of
the platform already uses.  No new infrastructure.

The store is split out behind a tiny interface so that:
- Tests can use a fake/in-memory implementation without Redis.
- Workers without Redis access can pass `None` for the store and the
  registry will degrade gracefully (no learned preferences, just static
  config + the default chain).
"""

from __future__ import annotations

import logging
from typing import Protocol
from urllib.parse import urlparse

logger = logging.getLogger(__name__)

_KEY_PREFIX = "fetcher:host:"


def host_of(uri: str) -> str | None:
    """Return a normalised hostname for `uri`, or None on parse failure."""
    try:
        host = urlparse(uri).netloc.lower()
    except Exception:
        return None
    return host or None


class HostPreferenceStore(Protocol):
    """Async-friendly key/value store of `host -> preferred_provider_id`."""

    async def get(self, host: str) -> str | None: ...

    async def record(self, host: str, provider_id: str) -> None: ...

    async def forget(self, host: str) -> None: ...


class InMemoryHostPreferenceStore:
    """Trivial dict-backed store, suitable for tests and single-process dev."""

    def __init__(self) -> None:
        self._data: dict[str, str] = {}

    async def get(self, host: str) -> str | None:
        return self._data.get(host)

    async def record(self, host: str, provider_id: str) -> None:
        self._data[host] = provider_id

    async def forget(self, host: str) -> None:
        self._data.pop(host, None)


class RedisHostPreferenceStore:
    """Redis-backed store with a configurable TTL.

    Uses the async redis client (`redis.asyncio.Redis`).  TTL defaults to
    30 days; we want preferences to age out so a temporary publisher outage
    doesn't permanently pin us to a heavy provider.
    """

    def __init__(self, redis: object, ttl_seconds: int = 60 * 60 * 24 * 30) -> None:
        self._redis = redis
        self._ttl = ttl_seconds

    async def get(self, host: str) -> str | None:
        try:
            value = await self._redis.get(_KEY_PREFIX + host)  # type: ignore[attr-defined]
        except Exception:
            logger.debug("redis host-pref get failed for %s", host, exc_info=True)
            return None
        if value is None:
            return None
        if isinstance(value, bytes):
            return value.decode("utf-8")
        return str(value)

    async def record(self, host: str, provider_id: str) -> None:
        try:
            await self._redis.set(_KEY_PREFIX + host, provider_id, ex=self._ttl)  # type: ignore[attr-defined]
        except Exception:
            logger.debug("redis host-pref record failed for %s", host, exc_info=True)

    async def forget(self, host: str) -> None:
        try:
            await self._redis.delete(_KEY_PREFIX + host)  # type: ignore[attr-defined]
        except Exception:
            logger.debug("redis host-pref forget failed for %s", host, exc_info=True)
