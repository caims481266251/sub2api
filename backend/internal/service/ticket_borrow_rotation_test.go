package service

import (
	"context"
	"encoding/base64"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type rotationTestRepo struct {
	*borrowWorkerRepo
	BorrowRotationRepository
	env    BorrowTargetEnvironment
	envErr error
	queued BorrowRotationInput
}

func (r *rotationTestRepo) TargetEnvironment(context.Context, int64) (BorrowTargetEnvironment, error) {
	return r.env, r.envErr
}
func (r *rotationTestRepo) QueueRotation(_ context.Context, in BorrowRotationInput, _ string) (*BorrowRotationJob, error) {
	r.queued = in
	return &BorrowRotationJob{ID: in.ID}, nil
}
func (r *rotationTestRepo) SaveRotationPolicy(context.Context, int64, BorrowRotationPolicy) error {
	return nil
}
func rotationServiceFixture() (*TicketBorrowService, *rotationTestRepo) {
	s, base := borrowFixture()
	base.route = nil
	r := &rotationTestRepo{borrowWorkerRepo: &borrowWorkerRepo{borrowTestRepo: base, source: boundBorrowTestSource(BorrowSource{ID: 1, Enabled: true, Model: "gpt-6-luna", Secret: BorrowSourceSecret{Credentials: map[string]any{"access_token": "source-token", "chatgpt_account_id": "source"}, DeviceID: "source-device"}})}, env: BorrowTargetEnvironment{EntryID: 7, Version: 1, DeviceID: "target-fixed-device", Proxy: "http://target.example:80"}}
	s.repo = r
	s.owner.openaiGatewayService = &OpenAIGatewayService{accountRepo: s.owner.accountRepo}
	return s, r
}
func rotationUpstream() *boundSourceUpstream {
	cookie := "x." + base64.RawURLEncoding.EncodeToString([]byte(`{"host":"chat.gateway.unified-1.api.openai.com"}`)) + ".x"
	mint := stateProbeMint("target-state")
	mint.cookies = nil
	return &boundSourceUpstream{stateProbeUpstream: &stateProbeUpstream{replies: []stateProbeReply{{status: 200, cookies: []string{"__oailb=" + cookie + "; Max-Age=230"}, body: stateProbeCompletedStream}, mint, {status: 200, body: stateProbeCompletedStream}}}}
}
func TestTicketBorrowCandidateEnvironmentIsIsolatedFromBusiness(t *testing.T) {
	s, r := rotationServiceFixture()
	a := s.owner.accountRepo.(borrowTestAccounts).account
	a.Proxy = &Proxy{Protocol: "http", Host: "ordinary.example", Port: 99}
	a.ProxyID = new(int64(99))
	up := rotationUpstream()
	s.owner.httpUpstream = up
	s.runTarget(t.Context(), BorrowTarget{AcquisitionIdentity: BorrowAcquisitionSource, AccountID: 11, Revision: 2}, BorrowSettings{Running: true, TTLSeconds: 230}, "worker")
	require.Len(t, up.calls, 3)
	require.NotNil(t, r.route)
	for i, call := range up.calls {
		require.Equal(t, "http://pool.example:80", call.proxy)
		require.Equal(t, up.calls[0].header.Get("X-Codex-Installation-ID"), call.header.Get("X-Codex-Installation-ID"))
		require.Equal(t, up.calls[0].header.Get("User-Agent"), call.header.Get("User-Agent"))
		require.Equal(t, up.profiles[0], up.profiles[i])
	}
	require.Equal(t, "Bearer source-token", up.calls[0].header.Get("Authorization"))
	require.Equal(t, "Bearer target-secret", up.calls[1].header.Get("Authorization"))
	req := borrowRequest(t)
	req.Header.Set("X-Codex-Installation-ID", "ordinary-device")
	normalProfile := &tlsfingerprint.Profile{Name: "ordinary-profile"}
	resp, err := s.ForwardHTTP(req, "", 11, 10, normalProfile, func(sent *http.Request, proxy string, _ int64, _ int, profile *tlsfingerprint.Profile) (*http.Response, error) {
		require.NoError(t, BorrowBeforeSend(sent))
		require.Equal(t, a.Proxy.URL(), proxy)
		require.Equal(t, "ordinary-device", sent.Header.Get("X-Codex-Installation-ID"))
		require.Same(t, normalProfile, profile)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody}, nil
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	r.env.Version++
	require.NoError(t, s.Check(t.Context(), 11, "gpt-6-astra", "http"), "legacy managed bindings do not define business identity")
	a.Proxy = &Proxy{Protocol: "http", Host: "new-ordinary.example", Port: 99}
	require.ErrorIs(t, s.Check(t.Context(), 11, "gpt-6-astra", "http"), ErrBorrowUnavailable)
}
func TestTicketBorrowOrdinaryTargetConfigurationFencesProbeAfterDial(t *testing.T) {
	s, r := rotationServiceFixture()
	up := rotationUpstream()
	s.owner.httpUpstream = up
	up.beforeSend = func() {
		if len(up.calls) == 1 {
			a := s.owner.accountRepo.(borrowTestAccounts).account
			a.Credentials = map[string]any{"access_token": "replacement", "chatgpt_account_id": "different"}
		}
	}
	s.runTarget(t.Context(), BorrowTarget{AcquisitionIdentity: BorrowAcquisitionSource, AccountID: 11, Revision: 2}, BorrowSettings{Running: true, TTLSeconds: 230}, "worker")
	require.Len(t, up.calls, 1)
	require.Nil(t, r.route)
}
func TestTicketBorrowRotationMutationsAreRetired(t *testing.T) {
	s, r := rotationServiceFixture()
	_, err := s.Rotate(t.Context(), BorrowRotationInput{ID: uuid.NewString(), Role: "source", AccountIDs: []int64{3, 1}})
	require.ErrorIs(t, err, ErrBorrowRotationRetired)
	require.Empty(t, r.queued.AccountIDs)
	require.ErrorIs(t, s.SaveRotationPolicy(t.Context(), 11, BorrowRotationPolicy{IntervalSeconds: 60, Automatic: true}), ErrBorrowRotationRetired)
}
