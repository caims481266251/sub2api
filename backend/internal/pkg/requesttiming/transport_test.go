package requesttiming

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTransportObservationIsolatedPerAttemptAndFrozen(t *testing.T) {
	c := New(time.Now(), 0)
	req, _ := http.NewRequestWithContext(With(t.Context(), c), "GET", "https://example.invalid", nil)
	req, _ = StartAttempt(req, 1, 2)
	ctx := WithTransport(req.Context(), TransportInfo{Kind: "utls", PoolID: "one"})
	physical, tr := StartTransport(req.WithContext(ctx))
	done := TransportStage(physical.Context(), "proxy_connect")
	done()
	done()
	TransportStage(physical.Context(), "raw-secret")()
	tr.Response(&http.Response{Proto: "HTTP/2.0", StatusCode: 200, TLS: &tls.ConnectionState{HandshakeComplete: true, NegotiatedProtocol: "h2", DidResume: true}}, nil)
	second, secondTrace := StartTransport(req.WithContext(WithTransport(req.Context(), TransportInfo{Kind: "net_http", PoolID: "two"})))
	_ = second
	secondTrace.Response(&http.Response{Proto: "HTTP/1.1", StatusCode: 200}, nil)
	c.Finish(200, false)
	WithTransport(physical.Context(), TransportInfo{PoolID: "too-late"})
	c.WhenFinished(func(s Snapshot) {
		require.Equal(t, "one", s.Attempts[1].Transport.PoolID)
		require.Equal(t, "two", s.Attempts[2].Transport.PoolID)
		require.Equal(t, "h2", s.Attempts[1].ALPN)
		require.Equal(t, "resumed", s.Attempts[1].TLSHandshake)
		require.Empty(t, s.Attempts[2].TLSHandshake)
		require.Contains(t, s.Attempts[1].Events, "proxy_connect_end")
		require.NotContains(t, s.Attempts[1].Events, "raw-secret_start")
	})
}

func TestIdentityDigestIgnoresCredentialsAndTracksIdentity(t *testing.T) {
	h := http.Header{}
	require.Empty(t, IdentityDigest(h))
	h.Set("User-Agent", "one")
	id := IdentityDigest(h)
	h.Set("Authorization", "Bearer secret")
	h.Set("Cookie", "secret")
	h.Set("X-Client-Request-Id", "per-request")
	require.Equal(t, id, IdentityDigest(h))
	h.Set("Version", "two")
	require.NotEqual(t, id, IdentityDigest(h))
}

func TestTransportObservationTLSOnWarmHTTP2(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	c := New(time.Now(), 0)
	for range 2 {
		req, _ := http.NewRequestWithContext(With(t.Context(), c), "GET", server.URL, nil)
		req, trace := StartAttempt(req, 1, 0)
		resp, err := server.Client().Do(req)
		trace.Response(resp, err)
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}
	c.Finish(200, false)
	c.WhenFinished(func(s Snapshot) {
		cold, warm := s.Attempts[0], s.Attempts[1]
		require.Equal(t, "HTTP/2.0", cold.Protocol)
		require.Equal(t, "h2", warm.ALPN)
		require.Equal(t, "full", warm.TLSHandshake)
		require.True(t, *warm.Reused)
		require.Contains(t, cold.Events, "tls_end")
		require.NotContains(t, warm.Events, "tls_start", "reused TLS has state, not another handshake")
	})
}
