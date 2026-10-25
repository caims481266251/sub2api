package tlsfingerprint

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/stretchr/testify/require"
)

func TestHTTPSProxyVerifiesOuterTLSAndPreservesInnerFingerprint(t *testing.T) {
	type observation struct {
		auth, target string
		ciphers      []uint16
	}
	seen := make(chan observation, 1)
	proxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodConnect {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		if rw.Flush() != nil {
			return
		}
		server := tls.Server(conn, &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			seen <- observation{req.Header.Get("Proxy-Authorization"), req.Host, hello.CipherSuites}
			return nil, errors.New("captured target ClientHello")
		}})
		_ = server.Handshake()
	}))
	defer proxy.Close()
	u, err := url.Parse(proxy.URL)
	require.NoError(t, err)
	u.User = url.UserPassword("borrow", "secret")
	profile := &Profile{CipherSuites: []uint16{0x1302, 0x1301, 0x1303, 0xc02f, 0xc030}}
	dialer := NewHTTPProxyDialer(profile, u)
	_, err = dialer.DialTLSContext(t.Context(), "tcp", "target.example:443")
	require.ErrorContains(t, err, "TLS handshake with proxy")
	select {
	case <-seen:
		t.Fatal("CONNECT credentials sent to an untrusted proxy")
	default:
	}
	roots := x509.NewCertPool()
	roots.AddCert(proxy.Certificate())
	dialer.proxyTLSConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	collector := requesttiming.New(time.Now(), 0)
	request, err := http.NewRequestWithContext(requesttiming.With(t.Context(), collector), "GET", "https://target.example", nil)
	require.NoError(t, err)
	request, _ = requesttiming.StartAttempt(request, 1, 2)
	_, err = dialer.DialTLSContext(request.Context(), "tcp", "target.example:443")
	require.Error(t, err, "capture server intentionally stops before completing target TLS")
	collector.Finish(502, false)
	collector.WhenFinished(func(s requesttiming.Snapshot) {
		e := s.Attempts[0].Events
		for _, name := range []string{"proxy_tls_start", "proxy_tls_end", "proxy_connect_start", "proxy_connect_end", "tls_start", "tls_end"} {
			require.Contains(t, e, name)
		}
		require.LessOrEqual(t, e["proxy_tls_end"], e["proxy_connect_start"])
		require.LessOrEqual(t, e["proxy_connect_end"], e["tls_start"])
		require.Empty(t, s.Attempts[0].TLSHandshake, "failed target handshake must not inherit successful proxy TLS")
	})
	select {
	case got := <-seen:
		require.Equal(t, "Basic Ym9ycm93OnNlY3JldA==", got.auth)
		require.Equal(t, "target.example:443", got.target)
		require.Equal(t, profile.CipherSuites, got.ciphers)
	case <-time.After(time.Second):
		t.Fatal("target fingerprint was not sent through the TLS proxy")
	}
}
