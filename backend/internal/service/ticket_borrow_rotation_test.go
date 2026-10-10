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
func TestTicketBorrowLegacyTargetEnvironmentStaysStableForProbesAndBusiness(t *testing.T) {
	s, r := rotationServiceFixture()
	up := rotationUpstream()
	s.owner.httpUpstream = up
	s.runTarget(t.Context(), BorrowTarget{AccountID: 11, Revision: 2}, BorrowSettings{Running: true, TTLSeconds: 230}, "worker")
	require.Len(t, up.calls, 3)
	require.NotNil(t, r.route)
	for _, call := range up.calls[1:] {
		require.Equal(t, r.env.Proxy, call.proxy)
		require.Equal(t, r.env.DeviceID, call.header.Get("X-Codex-Installation-ID"))
		require.Equal(t, r.env.DeviceID, call.header.Get("X-Codex-Window-ID"))
		require.Equal(t, "Bearer target-secret", call.header.Get("Authorization"))
	}
	require.GreaterOrEqual(t, len(up.profiles), 2)
	for _, profile := range up.profiles[len(up.profiles)-2:] {
		require.Equal(t, borrowSourceProfile(r.env.DeviceID), profile)
	}
	// Ordinary account edits do not silently change a borrowing environment.
	a := s.owner.accountRepo.(borrowTestAccounts).account
	a.Proxy = &Proxy{Protocol: "http", Host: "ordinary.example", Port: 99}
	a.ProxyID = new(int64(99))
	require.NoError(t, s.Check(t.Context(), 11, "gpt-6-astra", "http"))
	resp, e := s.ForwardHTTP(borrowRequest(t), "", 11, 10, nil, func(req *http.Request, proxy string, _ int64, _ int, profile *tlsfingerprint.Profile) (*http.Response, error) {
		require.NoError(t, BorrowBeforeSend(req))
		require.Equal(t, a.Proxy.URL(), proxy, "business uses the normal target proxy even with a legacy borrowing binding")
		require.Equal(t, r.env.DeviceID, req.Header.Get("X-Codex-Installation-ID"))
		require.Equal(t, borrowSourceProfile(r.env.DeviceID), profile)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody}, nil
	})
	require.NoError(t, e)
	require.NoError(t, resp.Body.Close())
	r.env.Version++
	require.ErrorIs(t, s.Check(t.Context(), 11, "gpt-6-astra", "http"), ErrBorrowUnavailable)
}
func TestTicketBorrowLegacyTargetEnvironmentFencesProbeAfterDial(t *testing.T) {
	s, r := rotationServiceFixture()
	up := rotationUpstream()
	s.owner.httpUpstream = up
	up.beforeSend = func() {
		if len(up.calls) == 1 {
			r.env.Version++
		}
	}
	s.runTarget(t.Context(), BorrowTarget{AccountID: 11, Revision: 2}, BorrowSettings{Running: true, TTLSeconds: 230}, "worker")
	require.Len(t, up.calls, 1, "a changed environment must prevent the target probe send")
	require.Nil(t, r.route)
	r.envErr = ErrBorrowUnavailable
	status, e := s.Status(t.Context(), 11)
	require.NoError(t, e)
	require.False(t, status.Ready)
	require.Equal(t, "target_proxy_pool_unavailable", status.Reason)
	r.envErr = ErrBorrowRotationPending
	status, e = s.Status(t.Context(), 11)
	require.NoError(t, e)
	require.Equal(t, "borrow_rotation_pending", status.Reason)
}
func TestTicketBorrowRotationValidatesAndNormalizesRequests(t *testing.T) {
	s, r := rotationServiceFixture()
	id := uuid.NewString()
	for _, in := range []BorrowRotationInput{{ID: "invalid", Role: "source", AccountIDs: []int64{1}}, {ID: id, Role: "all", AccountIDs: []int64{1}}, {ID: id, Role: "target", AccountIDs: []int64{11}}, {ID: id, Role: "source", AccountIDs: []int64{-1}}} {
		_, e := s.Rotate(t.Context(), in)
		require.ErrorIs(t, e, ErrBorrowRotationInvalid)
	}
	_, e := s.Rotate(t.Context(), BorrowRotationInput{ID: id, Role: "source", AccountIDs: []int64{3, 1, 3}})
	require.NoError(t, e)
	require.Equal(t, []int64{1, 3}, r.queued.AccountIDs)
	for _, n := range []int{-1, 0, 86401} {
		require.ErrorIs(t, s.SaveRotationPolicy(t.Context(), 11, BorrowRotationPolicy{IntervalSeconds: n}), ErrBorrowRotationInvalid)
	}
	require.NoError(t, s.SaveRotationPolicy(t.Context(), 11, BorrowRotationPolicy{IntervalSeconds: 60, Automatic: true}))
}
