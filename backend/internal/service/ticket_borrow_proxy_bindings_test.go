package service

import (
	"context"
	"encoding/base64"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type boundSourceUpstream struct {
	*stateProbeUpstream
	profiles   []*tlsfingerprint.Profile
	beforeSend func()
}

func (u *boundSourceUpstream) DoWithTLS(req *http.Request, proxy string, id int64, n int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	if u.beforeSend != nil {
		u.beforeSend()
	}
	if err := BorrowBeforeSend(req); err != nil {
		return nil, err
	}
	u.profiles = append(u.profiles, profile)
	return u.stateProbeUpstream.Do(req, proxy, id, n)
}

func TestBorrowPoolUsesStableFingerprintAndFencesChangedBinding(t *testing.T) {
	s, base := borrowFixture()
	a := s.owner.accountRepo.(borrowTestAccounts).account
	a.ID = 20
	proxyID := int64(99)
	a.ProxyID = &proxyID // Missing normal proxy does not invalidate a borrow-only binding.
	r := normalBorrowTestRepo{&borrowWorkerRepo{borrowTestRepo: base, source: BorrowSource{
		ID: 2, AccountID: 20, Enabled: true, Model: "gpt-6-luna", UseProxyPool: true, PoolEntryID: 7,
		Secret: BorrowSourceSecret{DeviceID: "persistent-device", Proxies: []string{"http://pool.example:80"}},
	}}}
	s.repo = r
	slot := &normalBorrowSlot{allow: true}
	s.owner.openaiGatewayService = &OpenAIGatewayService{accountRepo: s.owner.accountRepo, concurrencyService: &ConcurrencyService{cache: slot}}
	cookie := "x." + base64.RawURLEncoding.EncodeToString([]byte(`{"host":"chat.gateway.unified-1.api.openai.com"}`)) + ".x"
	reply := stateProbeReply{status: 200, cookies: []string{"__oailb=" + cookie + "; Max-Age=230"}, body: stateProbeCompletedStream}
	up := &boundSourceUpstream{stateProbeUpstream: &stateProbeUpstream{replies: []stateProbeReply{reply, reply}}}
	s.owner.httpUpstream = up
	for range 2 {
		_, _, err := s.acquireSource(t.Context(), r.source, BorrowSettings{TTLSeconds: 230})
		require.NoError(t, err)
	}
	require.Len(t, up.calls, 2)
	require.Equal(t, "http://pool.example:80", up.calls[0].proxy)
	require.Equal(t, "persistent-device", up.calls[0].header.Get("x-codex-installation-id"))
	for _, name := range []string{"x-codex-installation-id", "x-codex-window-id", "user-agent", "version"} {
		require.Equal(t, up.calls[0].header.Get(name), up.calls[1].header.Get(name))
	}
	require.Equal(t, up.profiles[0], up.profiles[1])
	require.NotNil(t, up.profiles[0])
	require.Equal(t, &proxyID, a.ProxyID)
	up.beforeSend = func() { r.source.PoolEntryID++ }
	_, _, err := s.acquireSource(t.Context(), r.source, BorrowSettings{TTLSeconds: 230})
	require.Error(t, err)
	require.Len(t, up.calls, 2, "a changed assignment must not send via the old proxy")
	r.source.PoolEntryID = 0
	_, _, err = s.acquireSource(t.Context(), r.source, BorrowSettings{TTLSeconds: 230})
	require.Error(t, err)
	require.Len(t, up.calls, 2, "a missing assignment must never fall back to direct egress")
}

type automaticBorrowRepo struct {
	*borrowWorkerRepo
	running bool
	jobs    int
}

func (r *automaticBorrowRepo) Cleanup(context.Context) error { return nil }
func (r *automaticBorrowRepo) Settings(context.Context) (BorrowSettings, error) {
	return BorrowSettings{Running: r.running, TTLSeconds: 230}, nil
}
func (r *automaticBorrowRepo) Targets(context.Context) ([]BorrowTarget, error) {
	return []BorrowTarget{{AccountID: 11, Enabled: true, Revision: 2}}, nil
}
func (r *automaticBorrowRepo) AcquireJob(_ context.Context, kind string, _ int64, _ string) (bool, error) {
	if kind == "target" {
		r.jobs++
	}
	return true, nil
}

func TestTicketBorrowAutomaticTickPauseValidRouteAndExpiry(t *testing.T) {
	s, base := borrowFixture()
	r := &automaticBorrowRepo{borrowWorkerRepo: &borrowWorkerRepo{borrowTestRepo: base, source: BorrowSource{ID: 1, Enabled: true, Model: "gpt-6-luna", Secret: BorrowSourceSecret{Credentials: map[string]any{"access_token": "source"}}}}}
	s.repo = r
	cookie := "x." + base64.RawURLEncoding.EncodeToString([]byte(`{"host":"chat.gateway.unified-1.api.openai.com"}`)) + ".x"
	mint := stateProbeMint("target-state")
	mint.cookies = nil
	up := &stateProbeUpstream{replies: []stateProbeReply{{status: 200, cookies: []string{"__oailb=" + cookie + "; Max-Age=230"}, body: stateProbeCompletedStream}, mint, {status: 200, body: stateProbeCompletedStream}}}
	s.owner.httpUpstream = up
	s.owner.openaiGatewayService = &OpenAIGatewayService{accountRepo: s.owner.accountRepo}
	s.tick(t.Context())
	s.wg.Wait()
	require.Zero(t, r.jobs)
	r.running = true
	s.tick(t.Context())
	s.wg.Wait()
	require.Zero(t, r.jobs, "valid routes stop background acquisition")
	base.now = base.route.ExpiresAt.Add(time.Millisecond)
	s.tick(t.Context())
	s.wg.Wait()
	require.Equal(t, 1, r.jobs)
	require.Len(t, up.calls, 3, "expiry should automatically acquire and verify without a manual refresh")
	require.NoError(t, s.Check(t.Context(), 11, "gpt-6-astra", "http"))
	s.tick(t.Context())
	s.wg.Wait()
	require.Equal(t, 1, r.jobs, "a newly verified route stops further acquisition")
}
