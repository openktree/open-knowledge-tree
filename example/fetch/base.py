"""Compatibility shim — the ABC now lives in ``kt_core_engine_api.fetch``.

Kept so existing ``from kt_providers.fetch.base import ContentFetcherProvider``
imports across the codebase keep working during the plugin-extraction
phase. New code should import from ``kt_core_engine_api.fetch`` directly.
"""

from kt_core_engine_api.fetch import ContentFetcherProvider

__all__ = ["ContentFetcherProvider"]
