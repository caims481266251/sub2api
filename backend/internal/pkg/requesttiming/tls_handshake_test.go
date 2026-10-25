package requesttiming

import (
	"crypto/tls"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTLSHandshakeKeepsRealDialerTiming(t *testing.T) {
	c := New(time.Now(), 0)
	req, err := http.NewRequestWithContext(With(t.Context(), c), "GET", "https://example.com", nil)
	require.NoError(t, err)
	_, attempt := StartAttempt(req, 1, 0)
	trace := attempt.clientTrace()
	trace.TLSHandshakeStart()
	trace.TLSHandshakeDone(tls.ConnectionState{}, nil)
	var start, end float64
	attempt.update(func(a *Attempt) { start, end = a.Events["tls_start"], a.Events["tls_end"] })
	// Go's transport inspects the already-completed custom TLS connection.
	trace.TLSHandshakeStart()
	trace.TLSHandshakeDone(tls.ConnectionState{}, nil)
	attempt.update(func(a *Attempt) {
		require.Equal(t, start, a.Events["tls_start"])
		require.Equal(t, end, a.Events["tls_end"])
	})
}
