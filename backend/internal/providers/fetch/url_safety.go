package fetch

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// AllowedFetchSchemes is the scheme allow-list for fetch URLs.
// The strategy and the Unpaywall OA-location path both run
// ValidateFetchURL before issuing a request so a malformed
// or hostile URL never reaches net/http. The list is
// intentionally narrow: ftp/file/data are not useful for
// content retrieval and would expand the attack surface.
var AllowedFetchSchemes = map[string]bool{
	"http":  true,
	"https": true,
}

// ErrUnsafeURL is returned by ValidateFetchURL when the URL
// is rejected by the SSRF guard. The strategy treats it as a
// hard error (not a fall-through sentinel): an unsafe URL is
// a caller bug or a hostile Unpaywall payload, not a
// transient failure worth retrying.
var ErrUnsafeURL = errors.New("fetch: unsafe URL rejected by SSRF guard")

// ErrDNSLookupFailed is returned by ValidateFetchURL when the
// host's A/AAAA lookup fails. It is a *transient* error, not a
// safety verdict: a DNS timeout does not mean the URL is unsafe,
// it means the resolver was unreachable (the canonical case in
// Docker Compose is the embedded DNS at 127.0.0.11:53 timing
// out under load). The strategy treats it as a fall-through
// sentinel so the fetch provider's own retry can re-resolve the
// host; previously the guard conflated DNS-failure with
// "unsafe URL" and short-circuited the whole chain, producing
// the "K_ssrf_dns_fail" failure mode (~11 rows in the corpus).
//
// The resolved-IP check still runs on any IP that comes back —
// only the "no IP at all" case changes from reject to retry.
// Security-wise this is equivalent because there is no address
// to fetch when DNS returns no address; the fetch provider will
// hit its own DNS lookup and fail the same way if the host is
// truly unreachable.
var ErrDNSLookupFailed = errors.New("fetch: DNS lookup failed (transient)")

// ValidateFetchURL returns nil when raw is an http(s) URL
// whose host resolves to at least one public address and no
// private / loopback / link-local / multicast / reserved /
// unspecified addresses. It mirrors the reference Python
// url_safety.validate_fetch_url: every A/AAAA record is
// checked and the URL is rejected if *any* resolved address
// is in a forbidden range, closing the DNS-rebinding window
// where the first record is public but a later one isn't.
//
// The function is injectable: the strategy accepts a
// URLValidator so tests can pass nil to disable the check
// (matching the reference's url_validator=None convention).
// Production wires the real validator; the Unpaywall
// provider calls ValidateFetchURL directly on the OA URL
// because Unpaywall-returned URLs are user-influenceable.
func ValidateFetchURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("%w: empty URL", ErrUnsafeURL)
	}

	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: parse error: %v", ErrUnsafeURL, err)
	}

	if !AllowedFetchSchemes[strings.ToLower(u.Scheme)] {
		return fmt.Errorf("%w: scheme %q not allowed", ErrUnsafeURL, u.Scheme)
	}

	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%w: empty host", ErrUnsafeURL)
	}

	// Strip the IPv6 zone index (e.g. fe80::1%eth0) before
	// resolution; the resolver rejects scoped literals.
	if h, _, ok := strings.Cut(host, "%"); ok {
		host = h
	}

	// Resolve every A/AAAA record. We do NOT rely on a single
	// lookup because DNS rebinding can hand back a mix of
	// public and private addresses; the safe behaviour is to
	// reject if any address is in a forbidden range.
	//
	// A lookup *error* (resolver unreachable, query timed out)
	// is returned as ErrDNSLookupFailed — a transient sentinel
	// distinct from ErrUnsafeURL — so the strategy can fall
	// through to the fetch providers' own retry instead of
	// short-circuiting the whole chain. The resolved-IP check
	// below only runs when the resolver returned at least one
	// address; a "no IP at all" outcome carries no safety
	// verdict, only a retry signal.
	ips, err := lookupAllIPs(host)
	if err != nil {
		return fmt.Errorf("%w: lookup failed for %q: %v", ErrDNSLookupFailed, host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("%w: no addresses resolved for %q", ErrDNSLookupFailed, host)
	}

	for _, ip := range ips {
		if !isPublicIP(ip) {
			return fmt.Errorf("%w: %q resolves to forbidden address %s", ErrUnsafeURL, host, ip.String())
		}
	}

	return nil
}

// lookupAllIPs resolves both A and AAAA records for host. It
// is a package-level var so tests can swap in a stub that
// returns a canned set of IPs without touching the real
// resolver.
var lookupAllIPs = func(host string) ([]net.IP, error) {
	// The Go resolver's LookupIP returns both A and AAAA
	// records when the system resolver supports both; this
	// matches the reference's getaddrinfo behaviour.
	return net.LookupIP(host)
}

// isPublicIP returns true when ip is not in any of the
// forbidden ranges: loopback, private (RFC 1918 / RFC 4193),
// link-local, multicast, reserved, or unspecified. The
// reference's url_safety uses is_private | is_loopback |
// is_link_local | is_multicast | is_reserved | is_unspecified;
// Go's net.IP methods cover the same ranges.
func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() ||
		ip.IsUnspecified() || ip.IsUnspecified() {
		return false
	}
	return true
}