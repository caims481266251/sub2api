package tlsfingerprint

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
)

func TestEffectiveProfilePreservesDialerSemantics(t *testing.T) {
	for _, profile := range []*Profile{nil, {}, {EnableGREASE: true}, {EnableGREASE: true, Extensions: []uint16{0, 43, 51}}, {CipherSuites: []uint16{0x1303, 0x1301}}} {
		effective := EffectiveProfile(profile)
		require.Equal(t, buildClientHelloSpecFromProfile(profile), buildClientHelloSpecFromProfile(effective))
		effective.CipherSuites[0] = 0
		require.NotZero(t, EffectiveProfile(profile).CipherSuites[0], "returned profile must own its slices")
	}
}

func TestBorrowProfilesCompleteRealTLS12AndTLS13Handshakes(t *testing.T) {
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		t.Run(tls.VersionName(version), func(t *testing.T) {
			observed := make(chan *tls.ClientHelloInfo, 1)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }))
			server.TLS = &tls.Config{MinVersion: version, MaxVersion: version, NextProtos: []string{"h2"}, GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
				observed <- hello
				return nil, nil
			}}
			server.EnableHTTP2 = true
			server.StartTLS()
			defer server.Close()
			roots := x509.NewCertPool()
			roots.AddCert(server.Certificate())
			for range 64 {
				p, err := NewBorrowProfile()
				require.NoError(t, err)
				tr := &http.Transport{ForceAttemptHTTP2: true, DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, addr)
					if err != nil {
						return nil, err
					}
					return handshakeFingerprint(ctx, conn, p, &utls.Config{ServerName: "example.com", RootCAs: roots})
				}}
				// Match the production transport registration, not just ALPN.
				h2, err := http2.ConfigureTransports(tr)
				require.NoError(t, err)
				h2.ReadIdleTimeout, h2.PingTimeout = 15*time.Second, 15*time.Second
				client := &http.Client{Transport: tr, Timeout: 3 * time.Second}
				for range 2 {
					resp, err := client.Get(server.URL)
					require.NoError(t, err)
					body, err := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					require.NoError(t, err)
					require.Equal(t, "ok", string(body))
					require.Equal(t, 2, resp.ProtoMajor)
					require.Equal(t, version, resp.TLS.Version)
					require.Equal(t, "h2", resp.TLS.NegotiatedProtocol)
				}
				hello := <-observed
				require.Equal(t, p.CipherSuites, hello.CipherSuites)
				require.Equal(t, p.SupportedVersions, hello.SupportedVersions)
				require.Equal(t, p.ALPNProtocols, hello.SupportedProtos)
				require.Empty(t, observed, "both requests reuse one actual H2 connection")
				tr.CloseIdleConnections()
			}
		})
	}
}
