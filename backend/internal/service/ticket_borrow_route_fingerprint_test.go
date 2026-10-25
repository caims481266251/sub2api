package service

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

func TestBorrowBusinessUsesSelectedRouteFingerprintAndAccountProxy(t *testing.T) {
	s, r := borrowFixture()
	a, err := s.owner.accountRepo.GetByID(t.Context(), 11)
	require.NoError(t, err)
	proxyID := int64(91)
	a.ProxyID = &proxyID
	a.Proxy = &Proxy{ID: proxyID, Protocol: "http", Host: "account-proxy.example", Port: 8080}
	r.route.Secret.Proxy = "http://candidate-proxy.example:9000"
	r.route.IdentityHash = s.targetIdentity(a)
	r.route.Secret.Headers.Set("Authorization", "Bearer stale-probe-credential")
	for _, routeID := range []string{"route-A", "route-B", "route-A-again"} {
		r.route.ID = routeID
		r.route.Secret.Fingerprint, err = NewBorrowFingerprint()
		require.NoError(t, err)
		_, err = s.ForwardHTTP(borrowRequest(t), "http://ignored.example", 11, 5, nil,
			func(req *http.Request, proxy string, id int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
				require.Equal(t, openAIAccountProxyURL(a), proxy)
				require.Equal(t, r.route.Secret.Fingerprint.Profile, profile)
				require.Equal(t, []string{"h2"}, profile.ALPNProtocols)
				require.Equal(t, r.route.Secret.Fingerprint.DeviceID, req.Header.Get("X-Codex-Installation-ID"))
				require.Equal(t, "Bearer target-secret", req.Header.Get("Authorization"))
				require.Equal(t, "conversation-ticket", req.Header.Get("X-Codex-Turn-State"))
				require.EqualValues(t, 11, id)
				require.Equal(t, 5, concurrency)
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody}, nil
			})
		require.NoError(t, err)
		require.Zero(t, activeBorrowLeases(r))
	}
}

func TestBorrowRouteFingerprintMissingNeverSendsOrLeaksLease(t *testing.T) {
	s, r := borrowFixture()
	r.route.Secret.Fingerprint = nil
	_, err := s.ForwardHTTP(borrowRequest(t), "", 11, 5, nil,
		func(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error) {
			t.Fatal("missing route snapshot must not use the account fingerprint")
			return nil, nil
		})
	require.ErrorIs(t, err, ErrBorrowFingerprintInvalid)
	require.Zero(t, activeBorrowLeases(r))
}

func TestBorrowRouteFingerprintSnapshotOwnsTLSFields(t *testing.T) {
	f, err := NewBorrowFingerprint()
	require.NoError(t, err)
	frozen, err := cloneBorrowFingerprint(f)
	require.NoError(t, err)
	require.Equal(t, f, frozen)
	f.Profile.CipherSuites[0] = 0
	f.Profile.ALPNProtocols[0] = "http/1.1"
	require.NoError(t, ValidateBorrowFingerprint(frozen))
	require.NotZero(t, frozen.Profile.CipherSuites[0])
	require.Equal(t, []string{"h2"}, frozen.Profile.ALPNProtocols)
}

func TestBorrowWSFingerprintClientsKeepIndependentRoutes(t *testing.T) {
	d := newDefaultOpenAIWSClientDialer().(*coderOpenAIWSClientDialer)
	f1, err := NewBorrowFingerprint()
	require.NoError(t, err)
	f2, err := NewBorrowFingerprint()
	require.NoError(t, err)
	a, err := d.borrowFingerprintHTTPClient("http://account.example:8080", f1.Profile)
	require.NoError(t, err)
	defer a.CloseIdleConnections()
	b, err := d.borrowFingerprintHTTPClient("http://account.example:8080", f2.Profile)
	require.NoError(t, err)
	defer b.CloseIdleConnections()
	again, err := d.borrowFingerprintHTTPClient("http://account.example:8080", f1.Profile)
	require.NoError(t, err)
	require.Same(t, a, again)
	require.NotSame(t, a, b)
	require.Equal(t, []string{"h2"}, f1.Profile.ALPNProtocols, "WS overlay must not mutate the stored HTTP fingerprint")
	headers := http.Header{"Authorization": {"Bearer target"}, "Session_id": {"business-session"}}
	ctx, err := prepareBorrowWSDial(t.Context(), headers, &BorrowRoute{Secret: BorrowRouteSecret{Fingerprint: f1}}, "gpt-6-astra")
	require.NoError(t, err)
	require.Equal(t, f1.Profile, ctx.Value(borrowWSFingerprintKey{}))
	require.Equal(t, f1.DeviceID, headers.Get("X-Codex-Installation-ID"))
	require.Equal(t, "Bearer target", headers.Get("Authorization"))
	require.Equal(t, "business-session", headers.Get("session_id"))
}
