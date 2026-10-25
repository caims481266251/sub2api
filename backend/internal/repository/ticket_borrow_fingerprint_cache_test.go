package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestBorrowFingerprintCacheChangesWithProfile(t *testing.T) {
	svc := NewHTTPUpstream(nil).(*httpUpstreamService)
	profile := &tlsfingerprint.Profile{Name: "borrow-device", CipherSuites: []uint16{0x1301, 0x1302, 0x1303}}
	first, err := svc.getClientEntryWithTLS("http://pool.example:80", 20, 1, profile, service.HTTPUpstreamProfileDefault, false, false)
	require.NoError(t, err)
	same, err := svc.getClientEntryWithTLS("http://pool.example:80", 20, 1, profile, service.HTTPUpstreamProfileDefault, false, false)
	require.NoError(t, err)
	require.Same(t, first, same, "a stable borrowing environment should reuse its transport")
	changed := &tlsfingerprint.Profile{Name: profile.Name, CipherSuites: []uint16{0x1302, 0x1301, 0x1303}}
	next, err := svc.getClientEntryWithTLS("http://pool.example:80", 20, 1, changed, service.HTTPUpstreamProfileDefault, false, false)
	require.NoError(t, err)
	require.NotSame(t, first, next, "the same profile name must not hide a changed TLS configuration")
	changed.Name = "borrow-new-device"
	newDevice, err := svc.getClientEntryWithTLS("http://pool.example:80", 20, 1, changed, service.HTTPUpstreamProfileDefault, false, false)
	require.NoError(t, err)
	require.NotSame(t, next, newDevice, "reassignment must use the new device's transport")
}
