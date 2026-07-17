"""Canonical URL + DOI extraction helpers.

Used by the multigraph public-cache machinery to look up sources across
graphs by a stable identity, independent of how the URL was typed by the
user (capitalisation, fragment, tracking params, etc.).

Two functions form the public surface:

* :func:`canonicalize_url` — produces a stable, comparable form of an
  arbitrary URL.  Conservative by design: it strips only well-known
  tracking parameters and never reorders the rest, because legitimate
  query params (``?id=123``, ``?article=foo``) are part of the resource's
  identity and dropping them would collapse distinct sources together.

* :func:`extract_doi` — returns the DOI for a URL, if any.  Checks the
  ``html_metadata`` dict first (the DOI provider already populates it,
  and HTML providers will populate it from ``<meta name="citation_doi">``
  via :func:`kt_providers.fetch.extract.extract_html_metadata`), then
  falls back to a regex over the URL itself.

Both helpers are pure and synchronous so they can be called from any
write site (worker pipelines, the API, tests) without taking sessions
or doing I/O.
"""

from __future__ import annotations

import re
from urllib.parse import parse_qsl, urlencode, urlsplit, urlunsplit

#: Conservative DOI regex — matches the canonical ``10.NNNN/anything``
#: pattern.  Shared with :mod:`kt_providers.fetch.doi_provider` which
#: imports it from here so the two definitions can never drift.
DOI_REGEX = re.compile(r"\b(10\.\d{4,9}/[^\s\"'<>]+)", re.IGNORECASE)

#: Tracking / analytics query parameters stripped during canonicalisation.
#: We deliberately keep this list short and well-known.  Anything not on
#: this list — including ``id``, ``page``, ``q``, ``article`` etc. — is
#: preserved because it can change which document the URL resolves to.
#: All comparisons are case-insensitive.
_TRACKING_PARAM_PREFIXES: tuple[str, ...] = (
    "utm_",
    "mc_",
)
_TRACKING_PARAM_EXACT: frozenset[str] = frozenset(
    {
        "gclid",
        "gclsrc",
        "dclid",
        "fbclid",
        "msclkid",
        "yclid",
        "igshid",
        "ref",
        "ref_src",
        "ref_url",
        "_ga",
        "_gl",
        "vero_id",
        "wickedid",
    }
)


def _is_tracking_param(name: str) -> bool:
    lname = name.lower()
    if lname in _TRACKING_PARAM_EXACT:
        return True
    return any(lname.startswith(p) for p in _TRACKING_PARAM_PREFIXES)


def canonicalize_url(uri: str) -> str:
    """Produce a stable form of ``uri`` suitable for cross-graph lookup.

    Transformations applied:

    * scheme lowercased
    * host (netloc) lowercased; default ports stripped (``:80`` for http,
      ``:443`` for https)
    * fragment dropped (``#...``) — fragments are client-only and never
      identify the resource
    * known tracking parameters removed (utm_*, gclid, fbclid, mc_*, ...).
      Order of remaining query params is preserved.
    * a trailing slash on a path-only URL (``https://example.com``) is
      *not* added or removed — that would change the request semantics on
      some servers.  Multiple consecutive slashes in the path collapse to
      one.

    The function is intentionally conservative: when in doubt it leaves
    the URL alone.  False positives (collapsing two distinct URLs to the
    same canonical form) cost us cache hits that turn into wrong content;
    false negatives (failing to collapse two equivalent URLs) only cost
    us a few duplicate decompositions.

    Returns the input unchanged when it does not parse as a URL with a
    scheme — bare strings, file paths, etc. fall through.
    """
    if not uri:
        return uri

    parts = urlsplit(uri)
    if not parts.scheme or not parts.netloc:
        return uri

    scheme = parts.scheme.lower()

    # Lowercase only the host portion of the netloc — userinfo (User:Pass@)
    # is case-sensitive in some auth schemes and must be preserved verbatim.
    # ``parts.hostname`` already returns the lowercased host; ``parts.username``
    # / ``parts.password`` / ``parts.port`` give us the rest.
    host = parts.hostname or ""
    netloc = host
    if parts.port is not None:
        # Strip default ports for the well-known schemes.
        if not ((scheme == "http" and parts.port == 80) or (scheme == "https" and parts.port == 443)):
            netloc = f"{netloc}:{parts.port}"
    if parts.username is not None:
        userinfo = parts.username
        if parts.password is not None:
            userinfo = f"{userinfo}:{parts.password}"
        netloc = f"{userinfo}@{netloc}"

    # Collapse runs of slashes in the path.  Preserves a leading single
    # slash but turns ``//foo///bar`` into ``/foo/bar``.
    path = re.sub(r"/{2,}", "/", parts.path)

    # Filter tracking params.  parse_qsl with keep_blank_values=True so we
    # don't drop params like ``?key=`` that some apps use as flags.
    query_pairs = parse_qsl(parts.query, keep_blank_values=True)
    filtered = [(k, v) for k, v in query_pairs if not _is_tracking_param(k)]
    query = urlencode(filtered, doseq=True)

    # Drop fragment unconditionally.
    return urlunsplit((scheme, netloc, path, query, ""))


def _doi_from_url(uri: str) -> str | None:
    """Pull a DOI directly out of a URL when possible.

    Two strategies, in order:

    1. ``doi.org`` / ``dx.doi.org`` URLs — the DOI is the path. The
       extracted candidate is validated against :data:`DOI_REGEX` so that
       non-DOI paths under doi.org (``/about``, ``/help``, etc.) return
       ``None`` instead of a bogus identifier.
    2. Any URL containing a DOI substring — match via :data:`DOI_REGEX`.

    Both branches strip trailing ``.`` and ``)`` so a DOI grabbed from
    prose ("see 10.1038/x.") or a URL with a stray sentence-end period
    yields a clean identifier.
    """
    if not uri:
        return None

    parts = urlsplit(uri)
    host = (parts.hostname or "").lower()
    if host in ("doi.org", "dx.doi.org", "www.doi.org"):
        candidate = parts.path.lstrip("/").rstrip(".)")
        if candidate and DOI_REGEX.fullmatch(candidate):
            return candidate
        # Fall through to the substring search below — the path didn't
        # look like a DOI but the rest of the URL might still contain one.

    m = DOI_REGEX.search(uri)
    if m:
        return m.group(1).rstrip(".)")

    return None


def extract_doi(
    uri: str,
    html_metadata: dict[str, str | None] | None = None,
) -> str | None:
    """Best-effort DOI extraction for a fetched URL.

    Resolution order:

    1. ``html_metadata["doi"]`` if present and non-empty.  Both the DOI
       provider (which sets it directly) and the HTML extractor (which
       reads ``<meta name="citation_doi">``) populate this key, so any
       fetcher that surfaces metadata will give us a DOI for free when
       the publisher provides one.
    2. URL-level extraction (see :func:`_doi_from_url`).

    Returns ``None`` when no DOI can be confidently extracted.  Callers
    should treat ``None`` as "this source has no canonical DOI identity"
    and rely on canonical URL alone for lookup.
    """
    if html_metadata:
        meta_doi = html_metadata.get("doi")
        if meta_doi:
            return meta_doi.strip() or None

    return _doi_from_url(uri)
