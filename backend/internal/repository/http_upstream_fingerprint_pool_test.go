package repository

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestTLSFingerprintPoolRetainsMultipleRouteClients(t *testing.T) {
	s := NewHTTPUpstream(&config.Config{}).(*httpUpstreamService)
	f1, err := tlsfingerprint.NewBorrowProfile()
	require.NoError(t, err)
	f2, err := tlsfingerprint.NewBorrowProfile()
	require.NoError(t, err)
	f1 = tlsfingerprint.PrepareHTTP2(f1)
	f2 = tlsfingerprint.PrepareHTTP2(f2)
	a, err := s.getClientEntryWithTLS("http://account.example:8080", 63, 5, f1, service.HTTPUpstreamProfileOpenAI, false, false)
	require.NoError(t, err)
	defer a.client.CloseIdleConnections()
	b, err := s.getClientEntryWithTLS("http://account.example:8080", 63, 5, f2, service.HTTPUpstreamProfileOpenAI, false, false)
	require.NoError(t, err)
	defer b.client.CloseIdleConnections()
	again, err := s.getClientEntryWithTLS("http://account.example:8080", 63, 5, f1, service.HTTPUpstreamProfileOpenAI, false, false)
	require.NoError(t, err)
	require.Same(t, a, again)
	require.NotSame(t, a, b)
	require.Len(t, s.clients, 2)
	changed := tlsfingerprint.CloneProfile(f1)
	changed.CipherSuites[0], changed.CipherSuites[1] = changed.CipherSuites[1], changed.CipherSuites[0]
	c, err := s.getClientEntryWithTLS("http://account.example:8080", 63, 5, changed, service.HTTPUpstreamProfileOpenAI, false, false)
	require.NoError(t, err)
	defer c.client.CloseIdleConnections()
	require.NotSame(t, a, c, "an edited prepared snapshot must never reuse the old transport")
	require.NotEqual(t, a.observation.FingerprintID, c.observation.FingerprintID)
	require.NotEqual(t, a.observation.PoolID, c.observation.PoolID)
	tr := a.client.Transport.(*http.Transport)
	require.False(t, tr.DisableKeepAlives)
	requireHTTP2Configured(t, tr, "custom TLS must attach a real H2 transport")
	for _, profile := range []service.HTTPUpstreamProfile{service.HTTPUpstreamProfileOpenAIHarvest, service.HTTPUpstreamProfileBorrowProbe} {
		require.Equal(t, upstreamProtocolModeOpenAIH2, s.resolveProtocolMode(profile, "", nil))
	}
}
