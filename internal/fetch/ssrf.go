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

// isPublicIP reports whether addr is publicly routable. IPv4-mapped IPv6
// addresses are unmapped first so ::ffff:169.254.169.254 and friends are
// judged by their embedded IPv4 value.
func isPublicIP(addr netip.Addr) bool {
	addr = addr.Unmap()
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
