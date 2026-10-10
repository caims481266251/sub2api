package tlsfingerprint

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/cryptobyte"
)

type borrowCapturedHello struct {
	ciphers, extensions []uint16
	err                 error
}

// Inspect real wire bytes, including extension ordering (ClientHelloInfo does
// not expose extension order). The capture endpoint intentionally ends TLS.
func captureBorrowHello(reader io.Reader) borrowCapturedHello {
	bad := func(err error) borrowCapturedHello { return borrowCapturedHello{err: err} }
	header := make([]byte, 5)
	if _, err := io.ReadFull(reader, header); err != nil {
		return bad(err)
	}
	if header[0] != 22 {
		return bad(fmt.Errorf("not a handshake record"))
	}
	buf := make([]byte, int(binary.BigEndian.Uint16(header[3:])))
	if _, err := io.ReadFull(reader, buf); err != nil {
		return bad(err)
	}
	s := cryptobyte.String(buf)
	var session, ciphers, compression, extensions cryptobyte.String
	if !s.Skip(4+2+32) || !s.ReadUint8LengthPrefixed(&session) || !s.ReadUint16LengthPrefixed(&ciphers) || !s.ReadUint8LengthPrefixed(&compression) || !s.ReadUint16LengthPrefixed(&extensions) {
		return bad(fmt.Errorf("malformed ClientHello"))
	}
	out := borrowCapturedHello{}
	for !ciphers.Empty() {
		var id uint16
		if !ciphers.ReadUint16(&id) {
			return bad(fmt.Errorf("malformed cipher list"))
		}
		out.ciphers = append(out.ciphers, id)
	}
	for !extensions.Empty() {
		var id uint16
		var data cryptobyte.String
		if !extensions.ReadUint16(&id) || !extensions.ReadUint16LengthPrefixed(&data) {
			return bad(fmt.Errorf("malformed extension"))
		}
		out.extensions = append(out.extensions, id)
	}
	return out
}

func TestBorrowProfileWireIdentityThroughProxyProtocols(t *testing.T) {
	for _, protocol := range []string{"http", "https", "socks5"} {
		t.Run(protocol, func(t *testing.T) {
			seen := make(chan borrowCapturedHello, 1)
			var proxyURL *url.URL
			var roots *x509.CertPool
			if protocol == "socks5" {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				require.NoError(t, err)
				defer listener.Close()
				proxyURL, err = url.Parse("socks5://" + listener.Addr().String())
				require.NoError(t, err)
				go func() {
					for {
						conn, err := listener.Accept()
						if err != nil {
							return
						}
						func() {
							defer conn.Close()
							_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
							err = acceptBorrowSOCKS(conn)
							if err != nil {
								seen <- borrowCapturedHello{err: err}
								return
							}
							seen <- captureBorrowHello(conn)
						}()
					}
				}()
			} else {
				server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if req.Method != http.MethodConnect {
						w.WriteHeader(405)
						return
					}
					conn, rw, err := w.(http.Hijacker).Hijack()
					if err != nil {
						seen <- borrowCapturedHello{err: err}
						return
					}
					defer conn.Close()
					_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
					_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
					if err = rw.Flush(); err != nil {
						seen <- borrowCapturedHello{err: err}
						return
					}
					seen <- captureBorrowHello(rw)
				}))
				if protocol == "https" {
					server.StartTLS()
					roots = x509.NewCertPool()
					roots.AddCert(server.Certificate())
				} else {
					server.Start()
				}
				defer server.Close()
				var err error
				proxyURL, err = url.Parse(server.URL)
				require.NoError(t, err)
			}
			for range 32 {
				p, err := NewBorrowProfile()
				require.NoError(t, err)
				if protocol == "socks5" {
					_, err = NewSOCKS5ProxyDialer(p, proxyURL).DialTLSContext(t.Context(), "tcp", "target.example:443")
				} else {
					dialer := NewHTTPProxyDialer(p, proxyURL)
					if roots != nil {
						dialer.proxyTLSConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
					}
					_, err = dialer.DialTLSContext(t.Context(), "tcp", "target.example:443")
				}
				require.Error(t, err, "capture endpoint intentionally closes after ClientHello")
				select {
				case hello := <-seen:
					require.NoError(t, hello.err)
					require.Equal(t, p.CipherSuites, hello.ciphers)
					require.Equal(t, p.Extensions, hello.extensions)
				case <-time.After(4 * time.Second):
					t.Fatal("no captured ClientHello")
				}
			}
		})
	}
}

func acceptBorrowSOCKS(conn net.Conn) error {
	var greeting [2]byte
	if _, err := io.ReadFull(conn, greeting[:]); err != nil {
		return err
	}
	if _, err := io.CopyN(io.Discard, conn, int64(greeting[1])); err != nil {
		return err
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return err
	}
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return err
	}
	n := 0
	switch header[3] {
	case 1:
		n = 4
	case 4:
		n = 16
	case 3:
		var size [1]byte
		if _, err := io.ReadFull(conn, size[:]); err != nil {
			return err
		}
		n = int(size[0])
	default:
		return fmt.Errorf("invalid SOCKS address")
	}
	if _, err := io.CopyN(io.Discard, conn, int64(n+2)); err != nil {
		return err
	}
	_, err := conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 80})
	return err
}
