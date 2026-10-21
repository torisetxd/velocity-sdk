package webfetch

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

const (
	// DefaultImpersonateProfile is the browser fingerprint used when
	// Options.Impersonate is set to a profile name that needs no choice or when
	// only Options.ImpersonateOnBlock is set.
	DefaultImpersonateProfile = "chrome_150"
	// DefaultImpersonateTimeout bounds a single impersonated fetch.
	DefaultImpersonateTimeout = 30 * time.Second
)

// chromeHeaderOrder is the header order a Chromium build sends. HTTP/2 header
// order is part of the fingerprint, so the adapter passes it to fhttp along
// with the pseudo-header order.
var chromeHeaderOrder = []string{
	"sec-ch-ua",
	"sec-ch-ua-mobile",
	"sec-ch-ua-platform",
	"upgrade-insecure-requests",
	"user-agent",
	"accept",
	"sec-fetch-site",
	"sec-fetch-mode",
	"sec-fetch-user",
	"sec-fetch-dest",
	"accept-encoding",
	"accept-language",
	"priority",
}

// ImpersonateClient returns a client that presents a real browser's TLS
// handshake, HTTP/2 settings and header order instead of Go's. Bot walls
// fingerprint the handshake rather than the user-agent header, so this is the
// lever that clears a managed challenge; a UA string cannot. profile is a name
// from profiles.MappedTLSClients ("chrome_150", "firefox_147", "safari_18_0",
// ...); empty means DefaultImpersonateProfile, and a timeout of zero means
// DefaultImpersonateTimeout.
//
// The client dials through the same blocked-address guard as NewSafeClient, so
// it stays safe for URLs supplied by a model or a user, and it keeps cookies
// across requests.
func ImpersonateClient(profile string, timeout time.Duration) (*http.Client, error) {
	name := strings.ToLower(strings.TrimSpace(profile))
	if name == "" {
		name = DefaultImpersonateProfile
	}
	clientProfile, ok := profiles.MappedTLSClients[name]
	if !ok {
		return nil, fmt.Errorf("webfetch: unknown tls profile %q", profile)
	}
	if timeout <= 0 {
		timeout = DefaultImpersonateTimeout
	}
	dialer := newGuardDialer(15 * time.Second)
	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(),
		tls_client.WithClientProfile(clientProfile),
		tls_client.WithCookieJar(tls_client.NewCookieJar()),
		tls_client.WithDialContext(dialer.DialContext),
		tls_client.WithCatchPanics(),
	)
	if err != nil {
		return nil, fmt.Errorf("webfetch: %w", err)
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &impersonateTransport{client: client},
	}, nil
}

// impersonateTransport adapts tls-client's fhttp client to the stdlib
// RoundTripper seam, so an impersonated fetch takes the exact same code path as
// a plain one.
type impersonateTransport struct {
	client tls_client.HttpClient
}

func (t *impersonateTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body io.Reader
	if req.Body != nil {
		defer req.Body.Close()
		body = req.Body
	}
	impersonated, err := fhttp.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), body)
	if err != nil {
		return nil, err
	}
	for key, values := range req.Header {
		for _, value := range values {
			impersonated.Header.Add(key, value)
		}
	}
	impersonated.Header[fhttp.PHeaderOrderKey] = []string{":method", ":authority", ":scheme", ":path"}
	impersonated.Header[fhttp.HeaderOrderKey] = chromeHeaderOrder
	impersonated.Host = req.Host

	resp, err := t.client.Do(impersonated)
	if err != nil {
		return nil, err
	}

	converted := &http.Response{
		Status:           resp.Status,
		StatusCode:       resp.StatusCode,
		Proto:            resp.Proto,
		ProtoMajor:       resp.ProtoMajor,
		ProtoMinor:       resp.ProtoMinor,
		Header:           make(http.Header, len(resp.Header)),
		Body:             resp.Body,
		ContentLength:    resp.ContentLength,
		TransferEncoding: resp.TransferEncoding,
		Uncompressed:     resp.Uncompressed,
		Request:          req,
	}
	for key, values := range resp.Header {
		if key == fhttp.HeaderOrderKey || key == fhttp.PHeaderOrderKey {
			continue
		}
		converted.Header[http.CanonicalHeaderKey(key)] = values
	}
	return converted, nil
}

func (t *impersonateTransport) CloseIdleConnections() {
	t.client.CloseIdleConnections()
}
