"""SSRF guard for the fetch provider chain.

`FetchProviderRegistry` calls `validate_fetch_url()` before handing a URL to
any provider, so a single chokepoint blocks the classic foot-guns of a
"knowledge graph that ingests user-supplied URLs":

- Non-http schemes (``file:///etc/passwd``, ``gopher://...``)
- Loopback / private / link-local / reserved IP literals
  (``http://127.0.0.1/``, ``http://10.0.0.1/``, ``http://169.254.169.254/``
  for AWS instance metadata, ``http://[::1]/``)
- Hostnames that resolve to any of the above (``http://localhost/``,
  internal DNS names that round-robin to a private VIP, etc.)

We resolve **every** A/AAAA record returned by getaddrinfo and reject if
*any* of them lands in an unsafe range — that closes a (small) window of
DNS rebinding where the first-resolved IP is public but a later one
isn't.  This is best-effort defense in depth; full DNS-rebinding immunity
would require a custom resolver inside every HTTP client we ship, which
is out of scope for this PR.

The validator is injected into `FetchProviderRegistry` rather than baked
into it so that:

- ``build_fetch_registry()`` always wires the real validator for production.
- Unit tests of the chain can pass ``url_validator=None`` and use any host
  they want without DNS roundtrips.
- Adversarial tests can target ``validate_fetch_url`` directly.
"""

from __future__ import annotations

import asyncio
import ipaddress
import logging
import socket
from urllib.parse import urlparse

logger = logging.getLogger(__name__)

ALLOWED_SCHEMES: frozenset[str] = frozenset({"http", "https"})


class UnsafeUrlError(ValueError):
    """Raised when a URL is rejected by the fetch URL safety check."""


async def validate_fetch_url(uri: str) -> None:
    """Validate `uri` for SSRF safety.

    Raises:
        UnsafeUrlError: If the scheme is not http(s), the URL has no
            hostname, DNS resolution fails, or *any* resolved address is
            loopback / private / link-local / multicast / reserved /
            unspecified.
    """
    try:
        parsed = urlparse(uri)
    except Exception as e:  # urlparse is forgiving but be defensive
        raise UnsafeUrlError(f"could not parse URL: {e}") from e

    scheme = (parsed.scheme or "").lower()
    if scheme not in ALLOWED_SCHEMES:
        raise UnsafeUrlError(f"scheme {scheme or '<empty>'!r} is not allowed (only http/https)")

    host = parsed.hostname
    if not host:
        raise UnsafeUrlError("URL has no hostname")

    try:
        loop = asyncio.get_running_loop()
        infos = await loop.getaddrinfo(host, None, type=socket.SOCK_STREAM)
    except socket.gaierror as e:
        raise UnsafeUrlError(f"DNS resolution failed for {host}: {e}") from e

    if not infos:
        raise UnsafeUrlError(f"no DNS records for {host}")

    for info in infos:
        sockaddr = info[4]
        ip_str = sockaddr[0]
        # IPv6 scoped addresses come back as "fe80::1%eth0" — strip the zone.
        if "%" in ip_str:
            ip_str = ip_str.split("%", 1)[0]
        try:
            ip = ipaddress.ip_address(ip_str)
        except ValueError as e:
            raise UnsafeUrlError(f"invalid IP {ip_str!r} for host {host}: {e}") from e

        if (
            ip.is_private
            or ip.is_loopback
            or ip.is_link_local
            or ip.is_multicast
            or ip.is_reserved
            or ip.is_unspecified
        ):
            raise UnsafeUrlError(f"refusing to fetch {host} → {ip_str} (private/loopback/reserved address)")
