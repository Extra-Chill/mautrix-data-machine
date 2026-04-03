package connector

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func hostFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return strings.TrimRight(rawURL, "/")
	}
	return u.Host
}

func httpClientWithTimeout(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
	}
}

// localClient returns an HTTP client that resolves all hostnames to 127.0.0.1,
// bypassing DNS/Cloudflare when the bridge runs on the same server as WordPress.
// The original Host header is preserved so nginx routes correctly.
func localClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				// Replace the hostname with localhost, keep the port.
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

func normalizeBaseURL(rawURL string) string {
	trimmed := strings.TrimSpace(rawURL)
	trimmed = strings.TrimRight(trimmed, "/")
	return trimmed
}

func generateRandomString(byteLength int) (string, error) {
	buf := make([]byte, byteLength)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func pkceChallenge(verifier string) string {
	hash := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(hash[:])
}

func resolveCallbackURL(callbackURL string) (string, error) {
	if callbackURL == "" {
		return "", fmt.Errorf("callback URL is required")
	}

	parsed, err := url.Parse(callbackURL)
	if err != nil {
		return "", fmt.Errorf("invalid callback URL: %w", err)
	}

	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("callback URL must include scheme and host")
	}

	return strings.TrimRight(parsed.String(), "/"), nil
}

func webhookURLFromCallback(callbackURL string) (string, error) {
	parsed, err := url.Parse(callbackURL)
	if err != nil {
		return "", fmt.Errorf("invalid callback URL: %w", err)
	}
	path := parsed.Path
	if path == "" {
		path = "/callback"
	}
	if strings.HasSuffix(path, "/callback") {
		parsed.Path = strings.TrimSuffix(path, "/callback") + "/webhook"
	} else {
		parsed.Path = strings.TrimRight(path, "/") + "/webhook"
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}
