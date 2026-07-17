"""Ephemeral binary blob store keyed by URI.

Used to pass image / PDF bytes from the fetch stage into the decomposition
pipeline without persisting large blobs in the database.  Lives next to the
fetcher providers because that's where the bytes originate, but is itself
content-fetcher-agnostic.
"""

from __future__ import annotations


class FileDataStore:
    """In-memory store for raw file data, keyed by URI."""

    def __init__(self) -> None:
        self._data: dict[str, bytes] = {}

    def store(self, uri: str, data: bytes) -> None:
        self._data[uri] = data

    def get(self, uri: str) -> bytes | None:
        return self._data.get(uri)

    def remove(self, uri: str) -> None:
        self._data.pop(uri, None)

    def clear(self) -> None:
        self._data.clear()

    def has(self, uri: str) -> bool:
        return uri in self._data
