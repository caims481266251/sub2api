package repository

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/stretchr/testify/require"
)

func TestUpstreamObservationTracksWarmConnectionWithoutSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer private-token", r.Header.Get("Authorization"))
		_, _ = w.Write([]byte("private-response"))
	}))
	defer server.Close()
	s := &httpUpstreamService{clients: make(map[string]*upstreamClientEntry)}
	c := requesttiming.New(time.Now(), 0)
	for range 2 {
		req, err := http.NewRequestWithContext(requesttiming.With(t.Context(), c), "GET", server.URL, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer private-token")
		req.Header.Set("Cookie", "private-cookie")
		req.Header.Set("User-Agent", "private-identity")
		req, trace := requesttiming.StartAttempt(req, 7, 0)
		resp, err := s.Do(req, "", 7, 2)
		trace.Response(resp, err)
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}
	c.Finish(200, false)
	c.WhenFinished(func(snapshot requesttiming.Snapshot) {
		require.Len(t, snapshot.Attempts, 4)
		first, second := snapshot.Attempts[1], snapshot.Attempts[3]
		require.Equal(t, "net_http", first.Transport.Kind)
		require.Equal(t, "direct", first.Transport.ProxyMode)
		require.Len(t, first.Transport.PoolID, 16)
		require.Len(t, first.Transport.IdentityID, 16)
		require.Equal(t, first.Transport, second.Transport)
		require.Equal(t, "HTTP/1.1", second.Protocol)
		require.NotNil(t, second.Reused)
		require.True(t, *second.Reused)
		require.NotContains(t, second.Events, "tls_start")
		require.Empty(t, second.TLSHandshake)
		require.Contains(t, snapshot.Attempts[0].Events, "client_acquire_end")
		raw, err := json.Marshal(snapshot)
		require.NoError(t, err)
		for _, secret := range []string{"private-token", "private-cookie", "private-identity", "private-response", server.URL} {
			require.NotContains(t, string(raw), secret)
		}
	})
	for _, entry := range s.clients {
		entry.client.CloseIdleConnections()
	}
}
