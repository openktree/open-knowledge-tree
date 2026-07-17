"""Compatibility shim — types now live in ``kt_core_engine_api.fetch``.

Kept so existing ``from kt_providers.fetch.types import FetchResult``
imports across the codebase keep working during the plugin-extraction
phase. New code should import from ``kt_core_engine_api.fetch`` directly.
"""

from kt_core_engine_api.fetch import (
    MIN_EXTRACTED_LENGTH,
    FetchAttempt,
    FetchResult,
)

__all__ = [
    "MIN_EXTRACTED_LENGTH",
    "FetchAttempt",
    "FetchResult",
]
