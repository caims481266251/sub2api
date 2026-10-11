package service

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func borrowTestCandidateContext(ctx context.Context) context.Context {
	return WithBorrowAcquisition(ctx, BorrowAcquisition{TargetID: 11, Revision: 2, Lease: "test-candidate"})
}

func (r *borrowWorkerRepo) AllocateCandidateEnvironment(_ context.Context, id, _ string) (*BorrowCandidateEnvironment, error) {
	if r.environmentError != nil {
		return nil, r.environmentError
	}
	f, err := NewBorrowFingerprint()
	if err != nil {
		return nil, err
	}
	env := &BorrowCandidateEnvironment{ID: id, EntryID: 7, Proxy: "http://pool.example:80", Fingerprint: f}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.environments == nil {
		r.environments = map[string]*BorrowCandidateEnvironment{}
	}
	r.environments[id] = env
	return env, nil
}
func (r *borrowWorkerRepo) StartCandidateEnvironment(_ context.Context, id string) (time.Time, time.Time, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.environments[id]
	e.StartedAt = r.now
	e.AdmissionUntil = r.now.Add(230 * time.Second)
	return e.StartedAt, e.AdmissionUntil, nil
}
func (r *borrowWorkerRepo) CheckCandidateEnvironment(ctx context.Context, _ string) error {
	if r.environmentCheck != nil {
		return r.environmentCheck()
	}
	return ctx.Err()
}
func (r *borrowWorkerRepo) MarkCandidateEnvironmentSent(ctx context.Context, _ string) error {
	return ctx.Err()
}
func (r *borrowWorkerRepo) ReleaseCandidateEnvironment(context.Context, string) error { return nil }
func (r *borrowWorkerRepo) QueueSourceAttempt(context.Context, int64, string) error   { return nil }
func (r *borrowWorkerRepo) GrantSourceAttempt(context.Context, int64, string) (bool, error) {
	return true, nil
}
func (r *borrowWorkerRepo) CancelSourceAttempt(context.Context, string) error    { return nil }
func (r *borrowWorkerRepo) SourceAttemptSent(context.Context, string) error      { return nil }
func (r *borrowWorkerRepo) SourceAttemptCompleted(context.Context, string) error { return nil }
func (r *borrowWorkerRepo) SourceHoldUntil(context.Context, int64) (*time.Time, error) {
	return nil, nil
}

func TestBorrowCandidateProxySessionAndIdentity(t *testing.T) {
	a := &Account{ID: 63, Credentials: map[string]any{"chatgpt_account_id": "workspace", "chatgpt_user_id": "user"}}
	b := &Account{ID: 64, Credentials: a.Credentials}
	require.Equal(t, borrowProxyIdentity(a), borrowProxyIdentity(b))
	b.Credentials = map[string]any{"chatgpt_account_id": "workspace", "chatgpt_user_id": "other"}
	require.NotEqual(t, borrowProxyIdentity(a), borrowProxyIdentity(b))
	a.Credentials = map[string]any{}
	b.Credentials = map[string]any{}
	require.NotEqual(t, borrowProxyIdentity(a), borrowProxyIdentity(b))
	proxy := "http://user-sid-12345678-t-5:password@proxy.example:80"
	first, randomized, err := RandomizeBorrowProxySession(proxy)
	require.NoError(t, err)
	require.True(t, randomized)
	second, _, err := RandomizeBorrowProxySession(proxy)
	require.NoError(t, err)
	require.NotEqual(t, first, second)
	u, err := url.Parse(first)
	require.NoError(t, err)
	password, _ := u.User.Password()
	require.Equal(t, "password", password)
	require.Equal(t, "proxy.example:80", u.Host)
	ordinary := "http://unchanged:password@proxy.example:80"
	got, randomized, err := RandomizeBorrowProxySession(ordinary)
	require.NoError(t, err)
	require.False(t, randomized)
	require.Equal(t, ordinary, got)
}

func TestBorrowCandidateAdmissionConsumesAcquisitionAndVerification(t *testing.T) {
	start := time.Now().Add(-12 * time.Second)
	deadline := borrowAdmissionDeadline(start, 80, time.Now().Add(time.Hour))
	require.WithinDuration(t, time.Now().Add(68*time.Second), deadline, time.Second)
	earlier := start.Add(25 * time.Second)
	require.Equal(t, earlier, borrowAdmissionDeadline(start, 80, earlier))
	w := 1
	s := BorrowSettings{AdmissionWindowSeconds: &w}
	require.NoError(t, ResolveBorrowAdmissionSettings(&s, BorrowSettings{TTLSeconds: 230}))
	require.Equal(t, 1, s.AdmissionSeconds())
	require.Equal(t, 30, s.TTLSeconds)
	require.Equal(t, 29, *s.MinRouteRemainingSeconds)
	p := BorrowSettings{Replenishment: &BorrowReplenishmentSettings{Enabled: true, Inventory: 4, HoldSeconds: 150, IntervalSeconds: 60}}
	NormalizeBorrowSettings(&p)
	require.Equal(t, 2, p.Replenishment.Inventory)
	require.Zero(t, p.Replenishment.HoldSeconds)
}

func TestBorrowCandidateUsageIsRecordedOnlyAfterHeaders(t *testing.T) {
	checked, sent := 0, 0
	ctx := context.WithValue(t.Context(), borrowBeforeSendKey{}, func(context.Context) error { checked++; return nil })
	ctx = context.WithValue(ctx, borrowSourceSentKey{}, func(context.Context) error { sent++; return nil })
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.test", nil)
	require.NoError(t, err)
	require.NoError(t, BorrowBeforeSend(req))
	require.NoError(t, BorrowBeforeSend(req))
	require.Equal(t, 2, checked)
	require.Zero(t, sent, "pre-send validation cannot consume proxy history")
	require.NoError(t, BorrowSourceRequestSent(req))
	require.Equal(t, 1, sent)
}
