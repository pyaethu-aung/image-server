package fetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// newTestClient builds a Client whose resolver answers from the given map
// and whose base dial always connects to the httptest server's listener, so
// "public" hostnames are exercised end-to-end with zero real network use.
func newTestClient(t *testing.T, ts *httptest.Server, answers map[string][]netip.Addr, maxBytes int64) *Client {
	t.Helper()
	dialer := &SafeDialer{
		Resolver: &fakeResolver{answers: answers},
		Base: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, ts.Listener.Addr().String())
		},
	}
	return newClient(5*time.Second, maxBytes, dialer)
}

func TestFetch(t *testing.T) {
	body := "fake image bytes"
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, body)
	})
	mux.HandleFunc("/redirect-ok", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://public.test/ok", http.StatusFound)
	})
	mux.HandleFunc("/redirect-internal", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://internal.test/", http.StatusFound)
	})
	mux.HandleFunc("/redirect-scheme", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "ftp://public.test/file", http.StatusFound)
	})
	mux.HandleFunc("/redirect-loop", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://public.test/redirect-loop", http.StatusFound)
	})
	mux.HandleFunc("/teapot", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, strings.Repeat("x", 100))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	answers := map[string][]netip.Addr{
		"public.test":   addrs("93.184.216.34"),
		"internal.test": addrs("10.0.0.5"),
	}

	tests := []struct {
		name       string
		url        string
		maxBytes   int64
		wantBody   string
		wantErr    error
		wantErrAny bool
	}{
		{
			name:     "happy path",
			url:      "http://public.test/ok",
			maxBytes: 1024,
			wantBody: body,
		},
		{
			name:     "body exactly at cap is allowed",
			url:      "http://public.test/ok",
			maxBytes: int64(len(body)),
			wantBody: body,
		},
		{
			name:     "redirect to a public host is followed",
			url:      "http://public.test/redirect-ok",
			maxBytes: 1024,
			wantBody: body,
		},
		{
			name:     "blocked host",
			url:      "http://internal.test/",
			maxBytes: 1024,
			wantErr:  ErrBlockedAddress,
		},
		{
			name:     "redirect to an internal host is blocked",
			url:      "http://public.test/redirect-internal",
			maxBytes: 1024,
			wantErr:  ErrBlockedAddress,
		},
		{
			name:     "redirect to a non-http scheme is blocked",
			url:      "http://public.test/redirect-scheme",
			maxBytes: 1024,
			wantErr:  ErrScheme,
		},
		{
			name:     "redirect loop hits the hop cap",
			url:      "http://public.test/redirect-loop",
			maxBytes: 1024,
			wantErr:  ErrTooManyRedirects,
		},
		{
			name:     "non-http scheme rejected before any dial",
			url:      "file:///etc/passwd",
			maxBytes: 1024,
			wantErr:  ErrScheme,
		},
		{
			name:     "ftp scheme rejected",
			url:      "ftp://public.test/file",
			maxBytes: 1024,
			wantErr:  ErrScheme,
		},
		{
			name:       "unparseable URL",
			url:        "http://bad url with spaces/",
			maxBytes:   1024,
			wantErrAny: true,
		},
		{
			name:       "non-200 status",
			url:        "http://public.test/teapot",
			maxBytes:   1024,
			wantErrAny: true,
		},
		{
			name:     "oversized body",
			url:      "http://public.test/big",
			maxBytes: 99,
			wantErr:  ErrTooLarge,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, ts, answers, tt.maxBytes)
			got, err := c.Fetch(t.Context(), tt.url)
			switch {
			case tt.wantErrAny:
				if err == nil {
					t.Fatal("Fetch() error = nil, want an error")
				}
				return
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Fetch() error = %v, want %v", err, tt.wantErr)
				}
				return
			case err != nil:
				t.Fatalf("Fetch() error = %v", err)
			}
			if string(got) != tt.wantBody {
				t.Errorf("Fetch() body = %q, want %q", got, tt.wantBody)
			}
		})
	}
}

// TestNew covers the production constructor; the returned client must carry
// the SSRF-guarded transport and the configured cap.
func TestNew(t *testing.T) {
	c := New(30*time.Second, 1024)
	if c.maxBytes != 1024 {
		t.Errorf("maxBytes = %d, want 1024", c.maxBytes)
	}
	if c.http.Timeout != 30*time.Second {
		t.Errorf("timeout = %v, want 30s", c.http.Timeout)
	}
	// The guard must reject loopback without any network traffic.
	if _, err := c.Fetch(t.Context(), "http://127.0.0.1/"); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("Fetch(loopback) error = %v, want %v", err, ErrBlockedAddress)
	}
}
