package fetch

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
)

func TestIsPublicIP(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		// IPv4 blocked ranges
		{"10.0.0.1", false},
		{"172.16.0.1", false},
		{"172.31.255.255", false},
		{"192.168.1.1", false},
		{"127.0.0.1", false},
		{"127.8.8.8", false},
		{"169.254.169.254", false}, // cloud metadata
		{"169.254.0.1", false},
		{"0.0.0.0", false},
		{"224.0.0.251", false}, // multicast
		// IPv6 blocked ranges
		{"::1", false},
		{"fe80::1", false},
		{"fc00::1", false},
		{"fd00:ec2::254", false}, // AWS IPv6 metadata (ULA)
		{"::", false},
		{"ff02::1", false}, // link-local multicast
		{"ff01::1", false}, // interface-local multicast
		{"ff0e::1", false}, // global multicast
		// IPv4-mapped IPv6 must be judged by the embedded IPv4
		{"::ffff:169.254.169.254", false},
		{"::ffff:10.0.0.1", false},
		{"::ffff:127.0.0.1", false},
		{"::ffff:8.8.8.8", true},
		// Public addresses
		{"8.8.8.8", true},
		{"93.184.216.34", true},
		{"2606:4700::6810:84e5", true},
		{"2001:4860:4860::8888", true},
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			if got := isPublicIP(netip.MustParseAddr(tt.addr)); got != tt.want {
				t.Errorf("isPublicIP(%s) = %v, want %v", tt.addr, got, tt.want)
			}
		})
	}
}

func TestIsPublicIPInvalid(t *testing.T) {
	if isPublicIP(netip.Addr{}) {
		t.Error("isPublicIP(zero Addr) = true, want false")
	}
}

// fakeResolver maps hostnames to fixed answers without touching DNS.
type fakeResolver struct {
	answers map[string][]netip.Addr
	err     error
}

func (f *fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.answers[host], nil
}

func addrs(ss ...string) []netip.Addr {
	out := make([]netip.Addr, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParseAddr(s)
	}
	return out
}

func TestSafeDialerDialContext(t *testing.T) {
	resolveErr := errors.New("resolver down")
	dialStopped := errors.New("dial intercepted")

	tests := []struct {
		name       string
		address    string
		resolver   *fakeResolver
		wantDial   string // expected pinned address handed to Base; "" = no dial
		wantErr    error
		wantErrAny bool
	}{
		{
			name:     "public host dials the validated IP, pinned",
			address:  "public.test:443",
			resolver: &fakeResolver{answers: map[string][]netip.Addr{"public.test": addrs("93.184.216.34")}},
			wantDial: "93.184.216.34:443",
			wantErr:  dialStopped,
		},
		{
			name:     "ipv4-mapped answer is unmapped before dialing",
			address:  "mapped.test:80",
			resolver: &fakeResolver{answers: map[string][]netip.Addr{"mapped.test": addrs("::ffff:8.8.8.8")}},
			wantDial: "8.8.8.8:80",
			wantErr:  dialStopped,
		},
		{
			name:     "private answer is blocked",
			address:  "internal.test:80",
			resolver: &fakeResolver{answers: map[string][]netip.Addr{"internal.test": addrs("10.0.0.5")}},
			wantErr:  ErrBlockedAddress,
		},
		{
			name:     "metadata IP is blocked",
			address:  "metadata.test:80",
			resolver: &fakeResolver{answers: map[string][]netip.Addr{"metadata.test": addrs("169.254.169.254")}},
			wantErr:  ErrBlockedAddress,
		},
		{
			name:    "mixed public+private answer is blocked entirely",
			address: "mixed.test:80",
			resolver: &fakeResolver{answers: map[string][]netip.Addr{
				"mixed.test": addrs("93.184.216.34", "192.168.0.10"),
			}},
			wantErr: ErrBlockedAddress,
		},
		{
			name:     "ipv6 loopback literal is blocked",
			address:  "[::1]:80",
			resolver: &fakeResolver{answers: map[string][]netip.Addr{"::1": addrs("::1")}},
			wantErr:  ErrBlockedAddress,
		},
		{
			name:     "resolver error propagates",
			address:  "down.test:80",
			resolver: &fakeResolver{err: resolveErr},
			wantErr:  resolveErr,
		},
		{
			name:       "empty resolver answer errors",
			address:    "nowhere.test:80",
			resolver:   &fakeResolver{answers: map[string][]netip.Addr{}},
			wantErrAny: true,
		},
		{
			name:       "address without port errors",
			address:    "public.test",
			resolver:   &fakeResolver{},
			wantErrAny: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var dialed string
			d := &SafeDialer{
				Resolver: tt.resolver,
				Base: func(_ context.Context, _, address string) (net.Conn, error) {
					dialed = address
					return nil, dialStopped
				},
			}

			_, err := d.DialContext(t.Context(), "tcp", tt.address)
			switch {
			case tt.wantErrAny:
				if err == nil {
					t.Fatal("DialContext() error = nil, want an error")
				}
			case !errors.Is(err, tt.wantErr):
				t.Fatalf("DialContext() error = %v, want %v", err, tt.wantErr)
			}
			if dialed != tt.wantDial {
				t.Errorf("dialed %q, want %q", dialed, tt.wantDial)
			}
		})
	}
}

// TestSafeDialerDefaults exercises the nil-Resolver and nil-Base default
// paths without network traffic: an IP literal resolves locally, and a
// canceled context stops net.Dialer before it dials.
func TestSafeDialerDefaults(t *testing.T) {
	d := &SafeDialer{}

	// Default resolver, loopback literal: blocked before any dial.
	if _, err := d.DialContext(t.Context(), "tcp", "127.0.0.1:80"); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("DialContext(loopback) error = %v, want %v", err, ErrBlockedAddress)
	}

	// Default base dialer, public literal, canceled context: the dialer
	// returns the context error without touching the network.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := d.DialContext(ctx, "tcp", "192.0.2.1:80"); err == nil {
		t.Error("DialContext(canceled ctx) error = nil, want context error")
	}
}
