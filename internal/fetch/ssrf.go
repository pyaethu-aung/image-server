// Package fetch downloads images from user-supplied URLs with an SSRF guard:
// only public http(s) destinations are allowed, every hostname resolution is
// validated against private/loopback/link-local/metadata ranges, and the
// connection is pinned to the validated IP so a DNS rebind between check and
// dial cannot redirect the request to an internal host.
package fetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// Sentinel errors returned by the SSRF guard and the client.
var (
	// ErrBlockedAddress means the URL's host resolved to an address that is
	// not publicly routable (private, loopback, link-local, metadata, ...).
	ErrBlockedAddress = errors.New("fetch: destination address is not publicly routable")
	// ErrScheme means the URL (or a redirect target) is not http or https.
	ErrScheme = errors.New("fetch: only http and https URLs are allowed")
	// ErrTooManyRedirects means the redirect hop cap was exceeded.
	ErrTooManyRedirects = errors.New("fetch: too many redirects")
	// ErrTooLarge means the response body exceeded the configured size cap.
	ErrTooLarge = errors.New("fetch: response body too large")
)

// Resolver resolves hostnames to IP addresses. *net.Resolver satisfies it;
// tests inject a fake so no SSRF test ever touches the network.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// DialFunc dials a network address. It matches net.Dialer.DialContext.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// extraBlockedV4 holds IPv4 ranges that are not publicly routable but that the
// netip predicates below do not classify (they only know RFC 1918 + a handful
// of special-use prefixes). Blocking them closes SSRF paths to shared/reserved
// space that cloud and carrier networks use internally.
var extraBlockedV4 = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT / shared address space (RFC 6598)
	netip.MustParsePrefix("192.0.0.0/24"),  // IETF protocol assignments (RFC 6890)
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking (RFC 2544)
	netip.MustParsePrefix("240.0.0.0/4"),   // reserved / class E incl. broadcast (RFC 1112)
}

// embeddedIPv4 decodes an IPv4 address carried inside an IPv6 address by a
// transition mechanism other than the IPv4-mapped form (which Unmap already
// handles): NAT64 (64:ff9b::/96), 6to4 (2002::/16), and the deprecated
// IPv4-compatible form (::a.b.c.d, i.e. ::/96). These forms let a resolver hand
// back a globally-scoped IPv6 address whose real target is an internal IPv4
// host (e.g. 64:ff9b::a9fe:a9fe → 169.254.169.254), so the guard must judge
// them by the embedded IPv4 value, not the IPv6 wrapper.
func embeddedIPv4(addr netip.Addr) (netip.Addr, bool) {
	if !addr.Is6() {
		return netip.Addr{}, false
	}
	b := addr.As16()
	switch {
	// NAT64 well-known prefix 64:ff9b::/96 — embedded IPv4 in the low 32 bits.
	case b[0] == 0x00 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b &&
		b[4] == 0 && b[5] == 0 && b[6] == 0 && b[7] == 0 &&
		b[8] == 0 && b[9] == 0 && b[10] == 0 && b[11] == 0:
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	// 6to4 2002::/16 — embedded IPv4 in bits 16..48.
	case b[0] == 0x20 && b[1] == 0x02:
		return netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), true
	// IPv4-compatible ::/96 (deprecated) — embedded IPv4 in the low 32 bits.
	// :: (unspecified) and ::1 (loopback) share this prefix but are already
	// blocked below on their own, so exclude them to avoid decoding 0.0.0.x.
	case b[0] == 0 && b[1] == 0 && b[2] == 0 && b[3] == 0 &&
		b[4] == 0 && b[5] == 0 && b[6] == 0 && b[7] == 0 &&
		b[8] == 0 && b[9] == 0 && b[10] == 0 && b[11] == 0 &&
		!addr.IsUnspecified() && !addr.IsLoopback():
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	}
	return netip.Addr{}, false
}

// isPublicIP reports whether addr is publicly routable. IPv6 addresses that
// embed an IPv4 target (IPv4-mapped via Unmap, plus NAT64/6to4/IPv4-compatible
// via embeddedIPv4) are reduced to that IPv4 first, so ::ffff:169.254.169.254,
// 64:ff9b::a9fe:a9fe, and ::a9fe:a9fe are all judged as 169.254.169.254.
func isPublicIP(addr netip.Addr) bool {
	addr = addr.Unmap()
	if v4, ok := embeddedIPv4(addr); ok {
		addr = v4
	}
	switch {
	case !addr.IsValid(),
		addr.IsUnspecified(),
		addr.IsLoopback(),
		addr.IsPrivate(),          // RFC 1918 + IPv6 ULA (fc00::/7)
		addr.IsLinkLocalUnicast(), // 169.254.0.0/16 (cloud metadata) + fe80::/10
		addr.IsLinkLocalMulticast(),
		addr.IsInterfaceLocalMulticast(),
		addr.IsMulticast():
		return false
	}
	for _, p := range extraBlockedV4 {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

// SafeDialer validates every resolved IP for a hostname and dials the
// validated IP directly (pinned), never re-resolving, so the SSRF check and
// the connection always agree on the destination.
type SafeDialer struct {
	// Resolver defaults to net.DefaultResolver.
	Resolver Resolver
	// Base performs the actual dial to the validated IP; defaults to a
	// net.Dialer with a sane timeout.
	Base DialFunc
}

// DialContext resolves the host in address, rejects the dial unless every
// resolved IP is publicly routable, then dials the first validated IP.
func (d *SafeDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("fetch: invalid address %q: %w", address, err)
	}

	resolver := d.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	addrs, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("fetch: resolve %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("fetch: resolve %q: no addresses", host)
	}
	// Every IP must be public: a mixed answer is an attack shape (the
	// resolver could steer a retry at the private one).
	for _, a := range addrs {
		if !isPublicIP(a) {
			return nil, fmt.Errorf("%w: %s resolves to %s", ErrBlockedAddress, host, a)
		}
	}

	base := d.Base
	if base == nil {
		base = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	}
	// Pin the connection to the validated IP; no second lookup can occur.
	return base(ctx, network, net.JoinHostPort(addrs[0].Unmap().String(), port))
}
