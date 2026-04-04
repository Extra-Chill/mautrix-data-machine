package wordpress

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HostFromURL extracts the host (with port) from a URL string.
func HostFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return strings.TrimRight(rawURL, "/")
	}
	return u.Host
}

// HTTPClientWithTimeout returns a standard HTTP client with the given timeout.
func HTTPClientWithTimeout(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
	}
}

// LocalClient returns an HTTP client that resolves all hostnames to 127.0.0.1,
// bypassing DNS/Cloudflare when the bridge runs on the same server as WordPress.
// The original Host header is preserved so nginx routes correctly.
func LocalClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				_, port, _ := net.SplitHostPort(addr)
				if port == "" {
					port = "443"
				}
				return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, "127.0.0.1:"+port)
			},
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec // localhost self-signed is fine
			},
		},
	}
}
