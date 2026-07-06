package fetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// maxRedirects caps redirect hops. Each hop re-enters SafeDialer, so a
// public host redirecting to an internal one is re-validated and blocked.
const maxRedirects = 5

// Client fetches user-supplied URLs through the SSRF guard. TLS verification
// is left at its secure default and must never be disabled.
type Client struct {
	http     *http.Client
	maxBytes int64
}

// New returns a Client with the given total request timeout and response
// body size cap in bytes.
func New(timeout time.Duration, maxBytes int64) *Client {
	return newClient(timeout, maxBytes, &SafeDialer{})
}

// newClient is the injectable constructor tests use to supply a SafeDialer
// with a fake resolver and base dial.
func newClient(timeout time.Duration, maxBytes int64, dialer *SafeDialer) *Client {
	return &Client{
		http: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				DialContext:         dialer.DialContext,
				TLSHandshakeTimeout: 10 * time.Second,
			},
			CheckRedirect: checkRedirect,
		},
		maxBytes: maxBytes,
	}
}

// checkRedirect enforces the hop cap and the scheme allowlist on every
// redirect target. The destination itself is re-validated by SafeDialer
// when the redirected request dials.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return ErrTooManyRedirects
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fmt.Errorf("%w: redirect to %q", ErrScheme, req.URL.Scheme)
	}
	return nil
}

// Fetch downloads rawURL and returns the response body, capped at the
// configured size. It returns ErrScheme for non-http(s) URLs, ErrTooLarge
// for oversized bodies, and errors wrapping ErrBlockedAddress when the SSRF
// guard rejects the destination (directly or via a redirect).
func (c *Client) Fetch(ctx context.Context, rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("fetch: parse URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%w: got %q", ErrScheme, u.Scheme)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("fetch: build request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %q: %w", u.Host, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %q: unexpected status %d", u.Host, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("fetch %q: read body: %w", u.Host, err)
	}
	if int64(len(body)) > c.maxBytes {
		return nil, fmt.Errorf("%w: exceeds %d bytes", ErrTooLarge, c.maxBytes)
	}
	return body, nil
}
