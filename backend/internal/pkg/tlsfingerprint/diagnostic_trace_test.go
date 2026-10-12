package tlsfingerprint

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http/httptrace"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFingerprintHandshakeEmitsDiagnosticTrace(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	done := make(chan struct{})
	go func() { defer close(done); buf := make([]byte, 8192); _, _ = server.Read(buf); _ = server.Close() }()
	started, ended := 0, 0
	var handshakeErr error
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		TLSHandshakeStart: func() { started++ }, TLSHandshakeDone: func(_ tls.ConnectionState, err error) { ended++; handshakeErr = err },
	})
	_, err := performTLSHandshake(ctx, client, nil, "example.invalid:443")
	require.Error(t, err)
	require.Equal(t, 1, started)
	require.Equal(t, 1, ended)
	require.Error(t, handshakeErr)
	<-done
	// The failed handshake closes the supplied connection.
	_, err = client.Write([]byte("closed"))
	require.Error(t, err)
	require.NotErrorIs(t, handshakeErr, io.ErrShortWrite)
}
