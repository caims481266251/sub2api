package tlsfingerprint

import (
	"crypto/tls"

	utls "github.com/refraction-networking/utls"
)

// Go 1.27's net/http accepts this interface for custom TLS connections. uTLS
// returns its own ConnectionState type, which net/http otherwise cannot see;
// without this adapter an h2 ClientHello can be followed by HTTP/1.1 bytes.
type fingerprintConn struct{ *utls.UConn }

func (c *fingerprintConn) ConnectionState() tls.ConnectionState {
	s := c.UConn.ConnectionState()
	return tls.ConnectionState{
		Version: s.Version, HandshakeComplete: s.HandshakeComplete, DidResume: s.DidResume,
		CipherSuite: s.CipherSuite, NegotiatedProtocol: s.NegotiatedProtocol,
		NegotiatedProtocolIsMutual: s.NegotiatedProtocolIsMutual, ServerName: s.ServerName,
		PeerCertificates: s.PeerCertificates, VerifiedChains: s.VerifiedChains,
		SignedCertificateTimestamps: s.SignedCertificateTimestamps, OCSPResponse: s.OCSPResponse,
		TLSUnique: s.TLSUnique,
	}
}
