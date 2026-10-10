package tlsfingerprint

import (
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
			server.TLS = &tls.Config{MinVersion: version, MaxVersion: version, NextProtos: []string{"http/1.1"}, GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
				observed <- hello
				return nil, nil
			}}
			server.StartTLS()
			defer server.Close()
			roots := x509.NewCertPool()
			roots.AddCert(server.Certificate())
			for range 64 {
				p, err := NewBorrowProfile()
				require.NoError(t, err)
				conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
				require.NoError(t, err)
				require.NoError(t, conn.SetDeadline(time.Now().Add(3*time.Second)))
				client := utls.UClient(conn, &utls.Config{ServerName: "example.com", RootCAs: roots}, utls.HelloCustom)
				require.NoError(t, client.ApplyPreset(buildClientHelloSpecFromProfile(p)))
				err = client.HandshakeContext(t.Context())
				if err != nil {
					_ = conn.Close()
				}
				require.NoError(t, err)
				state := client.ConnectionState()
				require.Equal(t, version, state.Version)
				require.Equal(t, "http/1.1", state.NegotiatedProtocol)
				hello := <-observed
				require.Equal(t, p.CipherSuites, hello.CipherSuites)
				require.Equal(t, p.SupportedVersions, hello.SupportedVersions)
				require.Equal(t, p.ALPNProtocols, hello.SupportedProtos)
				_, err = io.WriteString(client, "GET / HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n")
				require.NoError(t, err)
				body, err := io.ReadAll(client)
				_ = client.Close()
				require.NoError(t, err)
				require.Contains(t, string(body), "200 OK")
				require.Contains(t, string(body), "\r\n\r\nok")
			}
		})
	}
}
