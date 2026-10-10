package service

import (
	"context"
	"encoding/base64"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type borrowSignalRepo struct {
	*borrowWorkerRepo
	finishMu        sync.Mutex
	reason, outcome string
	delay, recovery time.Duration
}

type borrowDeadlineRepo struct {
	*borrowSignalRepo
	resultContextLive bool
}

func (r *borrowDeadlineRepo) RecordProbeOutcome(ctx context.Context, id int64, lease, reason string, delay time.Duration) error {
	r.resultContextLive = ctx.Err() == nil
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return r.borrowSignalRepo.RecordProbeOutcome(ctx, id, lease, reason, delay)
}

type borrowDeadlineUpstream struct {
	*stateProbeUpstream
	onTarget func()
}

func (u *borrowDeadlineUpstream) DoWithTLS(req *http.Request, proxy string, id int64, n int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	if id == 11 {
		<-req.Context().Done()
		if u.onTarget != nil {
			u.onTarget()
		}
		return nil, req.Context().Err()
	}
	return u.stateProbeUpstream.DoWithTLS(req, proxy, id, n, profile)
}

func TestTicketBorrowBatchDeadlineStillPersistsProbeOutcome(t *testing.T) {
	s, base := borrowFixture()
	servingRoute := base.route
	base.route = nil
	r := &borrowDeadlineRepo{borrowSignalRepo: &borrowSignalRepo{borrowWorkerRepo: &borrowWorkerRepo{borrowTestRepo: base, source: boundBorrowTestSource(BorrowSource{ID: 1, Enabled: true, Model: "gpt-6-luna", Secret: BorrowSourceSecret{Credentials: map[string]any{"access_token": "source-test"}}})}}}
	s.repo = r
	s.owner.openaiGatewayService = &OpenAIGatewayService{accountRepo: s.owner.accountRepo}
	cookie := "x." + base64.RawURLEncoding.EncodeToString([]byte(`{"host":"chat.gateway.unified-1.api.openai.com"}`)) + ".x"
	s.owner.httpUpstream = &borrowDeadlineUpstream{onTarget: func() { base.mu.Lock(); base.route = servingRoute; base.mu.Unlock() }, stateProbeUpstream: &stateProbeUpstream{replies: []stateProbeReply{{status: 200, cookies: []string{"__oailb=" + cookie + "; Max-Age=230"}, body: stateProbeCompletedStream}}}}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	s.runTarget(ctx, BorrowTarget{AccountID: 11, Revision: 2}, BorrowSettings{Running: true, TTLSeconds: 230}, "expired-work")
	require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	require.True(t, r.resultContextLive, "recovery persistence must survive the work deadline")
	require.Equal(t, "target_probe_inconclusive", r.outcome)
	require.Equal(t, "target_probe_inconclusive", r.reason, "an expired work context is not a storage outage")
	require.NoError(t, s.Check(t.Context(), 11, "gpt-6-astra", "http"))
}

func (r *borrowSignalRepo) FinishJob(_ context.Context, kind string, _ int64, _, reason string, delay time.Duration) error {
	r.finishMu.Lock()
	defer r.finishMu.Unlock()
	if kind == "target" || r.reason == "" {
		r.reason, r.delay = reason, delay
	}
	return nil
}
func (r *borrowSignalRepo) RecordProbeOutcome(_ context.Context, _ int64, _, reason string, delay time.Duration) error {
	r.outcome, r.recovery = reason, delay
	return nil
}

func TestTicketBorrowSemanticErrorsPersistWaitAndKeepValidRoute(t *testing.T) {
	for _, tc := range []struct {
		name, signal, sourceReason, targetReason string
		status                                   int
	}{
		{"quota", "usage_limit_reached", "source_rate_limited", "target_rate_limited", 200},
		{"auth", "authentication_error", "source_credentials_unavailable", "target_credentials_unavailable", 200},
		{"http quota", "usage_limit_reached", "source_http_429", "target_rate_limited", 429},
		{"http auth", "authentication_error", "source_http_401", "target_credentials_unavailable", 401},
		{"http denied", "permission_denied", "source_http_403", "target_credentials_unavailable", 403},
	} {
		for _, sourceFails := range []bool{true, false} {
			stage := "target"
			if sourceFails {
				stage = "source"
			}
			t.Run(tc.name+"/"+stage, func(t *testing.T) {
				s, base := borrowFixture()
				r := &borrowSignalRepo{borrowWorkerRepo: &borrowWorkerRepo{borrowTestRepo: base, source: boundBorrowTestSource(BorrowSource{ID: 1, Enabled: true, Model: "gpt-6-luna", Secret: BorrowSourceSecret{Credentials: map[string]any{"access_token": "source-test"}}})}}
				s.repo = r
				s.owner.openaiGatewayService = &OpenAIGatewayService{accountRepo: s.owner.accountRepo}
				cookie := "x." + base64.RawURLEncoding.EncodeToString([]byte(`{"host":"chat.gateway.unified-1.api.openai.com"}`)) + ".x"
				acquired := stateProbeReply{status: 200, cookies: []string{"__oailb=" + cookie + "; Max-Age=230"}, body: stateProbeCompletedStream}
				failed := stateProbeReply{status: tc.status, cookies: acquired.cookies, body: `data: {"type":"response.failed","response":{"error":{"type":"` + tc.signal + `","resets_in_seconds":700}}}` + "\n\n"}
				replies := []stateProbeReply{acquired, failed}
				if sourceFails {
					replies = []stateProbeReply{failed}
				}
				up := &stateProbeUpstream{replies: replies}
				s.owner.httpUpstream = up
				if !sourceFails {
					servingRoute := base.route
					base.route = nil
					s.owner.httpUpstream = &borrowSignalArrivalUpstream{stateProbeUpstream: up, onTarget: func() { base.mu.Lock(); base.route = servingRoute; base.mu.Unlock() }}
				}
				if sourceFails {
					c, _, err := s.acquireSource(t.Context(), r.source, BorrowSettings{TTLSeconds: 230})
					require.ErrorIs(t, err, ErrBorrowUnavailable)
					require.Nil(t, c, "a failed stream must not supply a candidate, even with a Cookie header")
					require.Equal(t, tc.sourceReason, r.reason)
				} else {
					s.runTarget(t.Context(), BorrowTarget{AccountID: 11, Revision: 2}, BorrowSettings{Running: true, TTLSeconds: 230}, "signal-worker")
					require.Equal(t, tc.targetReason, r.reason)
					require.Equal(t, tc.targetReason, r.outcome)
					require.Equal(t, 700*time.Second, r.recovery)
				}
				if sourceFails {
					require.Zero(t, r.delay)
				} else {
					require.Equal(t, 700*time.Second, r.delay)
				}
				require.Len(t, up.calls, len(replies))
				require.Equal(t, "route-a", base.route.ID)
				require.NoError(t, s.Check(t.Context(), 11, "gpt-6-astra", "http"))
			})
		}
	}
}

// A concurrently published route must survive a late failure from older work.
type borrowSignalArrivalUpstream struct {
	*stateProbeUpstream
	onTarget func()
}

func (u *borrowSignalArrivalUpstream) DoWithTLS(req *http.Request, proxy string, id int64, n int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	if id == 11 {
		u.onTarget()
	}
	return u.stateProbeUpstream.DoWithTLS(req, proxy, id, n, profile)
}
