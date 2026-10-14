package repository

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAIHTTP1HostsNegotiationAndPoolIsolation(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	upstream := NewHTTPUpstream(&config.Config{Gateway: config.GatewayConfig{
		OpenAIHTTP2: config.GatewayOpenAIHTTP2Config{Enabled: true, HTTP1Hosts: []string{" H1.Example.test. "}},
	}}).(*httpUpstreamService)
	// Only the test transports dial loopback and trust the test certificate.
	// Do still chooses the real protocol pool using each request's hostname.
	for _, override := range []string{"", upstreamProtocolModeOpenAIH1} {
		entry, err := upstream.getClientEntry("", 14, 2, service.HTTPUpstreamProfileOpenAI, false, false, override)
		require.NoError(t, err)
		transport := entry.client.Transport.(*http.Transport)
		roots := x509.NewCertPool()
		roots.AddCert(server.Certificate())
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: "127.0.0.1"}
		transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		}
		defer transport.CloseIdleConnections()
	}
	for _, tc := range []struct {
		host  string
		major int
	}{
		{"h2.example.test", 2},
		{"h1.example.test", 1},
		{"sub.h1.example.test", 2},
		{"h1.example.test:9443", 1},
		{"h2.example.test", 2},
	} {
		t.Run(tc.host, func(t *testing.T) {
			ctx := service.WithHTTPUpstreamProfile(t.Context(), service.HTTPUpstreamProfileOpenAI)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+tc.host+"/v1/responses", nil)
			require.NoError(t, err)
			resp, err := upstream.Do(req, "", 14, 2)
			require.NoError(t, err)
			_, err = io.Copy(io.Discard, resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, tc.major, resp.ProtoMajor, "verify wire protocol, not only transport settings")
		})
	}
	require.Len(t, upstream.clients, 2, "overrides must not replace or repeatedly rebuild the default H2 pool")
}

func TestOpenAIHTTP1HostsDoesNotOverrideOtherProfiles(t *testing.T) {
	upstream := NewHTTPUpstream(&config.Config{Gateway: config.GatewayConfig{
		OpenAIHTTP2: config.GatewayOpenAIHTTP2Config{Enabled: true, HTTP1Hosts: []string{"h1.example.test"}},
	}}).(*httpUpstreamService)
	req, err := http.NewRequest(http.MethodPost, "https://h1.example.test/v1/responses", nil)
	require.NoError(t, err)
	for _, profile := range []service.HTTPUpstreamProfile{
		service.HTTPUpstreamProfileDefault, service.HTTPUpstreamProfileLongStream,
		service.HTTPUpstreamProfileBorrowProbe, service.HTTPUpstreamProfileOpenAIHarvest,
	} {
		require.Empty(t, upstream.requestProtocolOverride(req, profile))
	}
}
