package service

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

func TestBorrowReplenishmentSettingsAreBoundedAndOptIn(t *testing.T) {
	var absent *BorrowReplenishmentSettings
	require.NoError(t, absent.Validate())
	require.False(t, absent.Allows(63))
	p := BorrowReplenishmentSettings{Inventory: 2, IntervalSeconds: 10, HoldSeconds: 150}
	require.NoError(t, p.Validate())
	require.False(t, p.Allows(63))
	p.Enabled = true
	require.NoError(t, p.Validate())
	require.True(t, p.Allows(63))
	require.True(t, p.Allows(64))
	require.False(t, p.Allows(0))
	require.False(t, p.Allows(-1))
	for _, invalid := range []BorrowReplenishmentSettings{
		{Inventory: 2, IntervalSeconds: 10, HoldSeconds: 150, Shadow: true},
		{Inventory: 5, IntervalSeconds: 10, HoldSeconds: 150},
		{Inventory: 2, IntervalSeconds: 0, HoldSeconds: 150},
		{Inventory: 2, IntervalSeconds: 10, HoldSeconds: 151},
	} {
		require.Error(t, invalid.Validate())
	}
}

type replenishmentWorkerRepo struct {
	*prefetchReleaseRepo
	state    *BorrowReplenishmentState
	ended    string
	shadow   *BorrowRoute
	outcomes []string
}

func (r *replenishmentWorkerRepo) RecordProbeOutcome(_ context.Context, _ int64, _, reason string, _ time.Duration) error {
	r.outcomes = append(r.outcomes, reason)
	return nil
}

func (r *replenishmentWorkerRepo) CheckAcquisition(ctx context.Context, a BorrowAcquisition) error {
	if r.state != nil {
		return nil
	}
	return r.prefetchReleaseRepo.CheckAcquisition(ctx, a)
}
func (r *replenishmentWorkerRepo) Replenishment(context.Context, int64, string) (*BorrowReplenishmentState, error) {
	return r.state, nil
}
func (r *replenishmentWorkerRepo) EndReplenishment(_ context.Context, _ BorrowAcquisition, _ int64, reason string) error {
	r.ended = reason
	return nil
}
func (r *replenishmentWorkerRepo) Publish(ctx context.Context, route *BorrowRoute, lease string) error {
	if route.Shadow {
		r.shadow = route
		return nil
	}
	return r.prefetchReleaseRepo.Publish(ctx, route, lease)
}

type replenishmentUpstream struct {
	HTTPUpstream
	slowStarted chan struct{}
	cancelled   atomic.Int32
	targetCalls atomic.Int32
	degraded    bool
	single      bool
	blockTarget bool
}

func (u *replenishmentUpstream) DoWithTLS(req *http.Request, _ string, id int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	if id == 21 || id == 22 {
		u.slowStarted <- struct{}{}
		<-req.Context().Done()
		u.cancelled.Add(1)
		return nil, req.Context().Err()
	}
	h := http.Header{}
	if id == 23 {
		if !u.single {
			select {
			case <-u.slowStarted:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
		cookie := "x." + base64.RawURLEncoding.EncodeToString([]byte(`{"host":"chat.gateway.unified-3.api.openai.com"}`)) + ".x"
		h.Set("Set-Cookie", "__oailb="+cookie+"; Max-Age=230")
	} else {
		u.targetCalls.Add(1)
		if u.blockTarget {
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		if req.Header.Get(openAICodexTurnStateHeader) == "" {
			h.Set(openAICodexTurnStateHeader, "target-ticket")
		} else if u.degraded {
			h.Set(openAICodexTurnStateHeader, "changed-ticket")
		}
	}
	return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(stateProbeCompletedStream))}, nil
}

func TestTicketBorrowReplenishmentRetainedWorkerDoesNotWaitForExploration(t *testing.T) {
	for _, published := range []bool{true} {
		t.Run(map[bool]string{true: "published"}[published], func(t *testing.T) {
			s, base := borrowFixture()
			base.route = nil
			r := &replenishmentWorkerRepo{prefetchReleaseRepo: &prefetchReleaseRepo{observationWorkerRepo: &observationWorkerRepo{borrowWorkerRepo: &borrowWorkerRepo{borrowTestRepo: base}}}, state: &BorrowReplenishmentState{SourceID: 3, Until: time.Now().Add(20 * time.Second), Attempts: 1, IntervalSeconds: 10}}
			s.repo = r
			u := &replenishmentUpstream{slowStarted: make(chan struct{}, 2)}
			s.owner.httpUpstream = u
			s.owner.openaiGatewayService = &OpenAIGatewayService{accountRepo: s.owner.accountRepo}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			s.runTarget(ctx, BorrowTarget{AccountID: 11, Revision: 2, ProbePolicy: BorrowProbePolicy{Concurrency: 2, RenewBeforeSeconds: 15}}, BorrowSettings{Running: true, TTLSeconds: 230}, "replenish")
			require.NoError(t, ctx.Err(), "retained source must run alongside the slow alternate")
			require.EqualValues(t, 1, u.cancelled.Load())
			require.EqualValues(t, 2, u.targetCalls.Load(), "each fresh candidate needs mint and continuation")
			require.NotNil(t, base.route)
			for _, e := range base.events {
				require.NotEqual(t, "source_failed", e.Reason)
			}
		})
	}
}

func TestTicketBorrowReplenishmentMismatchPreservesOldRoute(t *testing.T) {
	s, base := borrowFixture()
	old := base.route
	r := &replenishmentWorkerRepo{prefetchReleaseRepo: &prefetchReleaseRepo{observationWorkerRepo: &observationWorkerRepo{borrowWorkerRepo: &borrowWorkerRepo{borrowTestRepo: base}}}, state: &BorrowReplenishmentState{SourceID: 3, Until: time.Now().Add(10 * time.Second), Attempts: 1, IntervalSeconds: 10}}
	s.repo = r
	s.owner.httpUpstream = &replenishmentUpstream{slowStarted: make(chan struct{}, 1), degraded: true, single: true}
	s.owner.openaiGatewayService = &OpenAIGatewayService{accountRepo: s.owner.accountRepo}
	s.runTarget(t.Context(), BorrowTarget{AccountID: 11, Revision: 2, ProbePolicy: BorrowProbePolicy{Concurrency: 1, RenewBeforeSeconds: 15}}, BorrowSettings{Running: true, TTLSeconds: 230}, "mismatch")
	require.Equal(t, "target_ticket_changed", r.ended)
	require.Same(t, old, base.route)
}

func TestTicketBorrowReplenishmentSingleWorkerExplorationIsBounded(t *testing.T) {
	s, base := borrowFixture()
	base.route = nil
	r := &replenishmentWorkerRepo{prefetchReleaseRepo: &prefetchReleaseRepo{observationWorkerRepo: &observationWorkerRepo{borrowWorkerRepo: &borrowWorkerRepo{borrowTestRepo: base}}}, state: &BorrowReplenishmentState{SourceID: 3, Until: time.Now().Add(20 * time.Second), Attempts: 2, IntervalSeconds: 1}}
	s.repo = r
	u := &replenishmentUpstream{slowStarted: make(chan struct{}, 2)}
	s.owner.httpUpstream = u
	s.owner.openaiGatewayService = &OpenAIGatewayService{accountRepo: s.owner.accountRepo}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	s.runTarget(ctx, BorrowTarget{AccountID: 11, Revision: 2, ProbePolicy: BorrowProbePolicy{Concurrency: 1, RenewBeforeSeconds: 15}}, BorrowSettings{Running: true, TTLSeconds: 230}, "exploration")
	require.NoError(t, ctx.Err())
	require.EqualValues(t, 1, u.cancelled.Load())
	require.Zero(t, u.targetCalls.Load())
}

func TestTicketBorrowReplenishmentWindowCancellationIsNotFailure(t *testing.T) {
	s, base := borrowFixture()
	base.route = nil
	r := &replenishmentWorkerRepo{prefetchReleaseRepo: &prefetchReleaseRepo{observationWorkerRepo: &observationWorkerRepo{borrowWorkerRepo: &borrowWorkerRepo{borrowTestRepo: base}}}, state: &BorrowReplenishmentState{SourceID: 3, Until: time.Now().Add(200 * time.Millisecond), Attempts: 1, IntervalSeconds: 10}}
	s.repo = r
	u := &replenishmentUpstream{slowStarted: make(chan struct{}, 1), single: true, blockTarget: true}
	s.owner.httpUpstream = u
	s.owner.openaiGatewayService = &OpenAIGatewayService{accountRepo: s.owner.accountRepo}
	ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
	defer cancel()
	s.runTarget(ctx, BorrowTarget{AccountID: 11, Revision: 2, ProbePolicy: BorrowProbePolicy{Concurrency: 1, RenewBeforeSeconds: 15}}, BorrowSettings{Running: true, TTLSeconds: 230}, "window-cutoff")
	require.EqualValues(t, 1, u.targetCalls.Load())
	require.Empty(t, r.outcomes, "window cancellation must not increment failure recovery")
	found := false
	for _, event := range base.events {
		if event.Reason == "candidate_unused_after_stop" {
			found = true
		}
		require.NotEqual(t, "target_continue_timeout", event.Reason)
	}
	require.True(t, found)
}

type continuousTestRepo struct {
	*replenishmentWorkerRepo
	published []*BorrowRoute
}

func (r *continuousTestRepo) Publish(_ context.Context, route *BorrowRoute, _ string) error {
	r.published = append(r.published, route)
	return nil
}

type continuousTestUpstream struct{ HTTPUpstream }

func (u continuousTestUpstream) DoWithTLS(req *http.Request, _ string, id int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	h := http.Header{}
	if id >= 21 {
		if id != 23 {
			select {
			case <-time.After(120 * time.Millisecond):
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
		host := "1"
		if id != 23 {
			host = "2"
		}
		value := "x." + base64.RawURLEncoding.EncodeToString([]byte(`{"host":"chat.gateway.unified-`+host+`.api.openai.com"}`)) + ".x"
		h.Set("Set-Cookie", "__oailb="+value+"; Max-Age=230")
	} else {
		// Verification continues after the source's 60ms preference has expired.
		select {
		case <-time.After(50 * time.Millisecond):
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
		if req.Header.Get(openAICodexTurnStateHeader) == "" {
			h.Set(openAICodexTurnStateHeader, "ticket")
		}
	}
	return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(stateProbeCompletedStream))}, nil
}
func TestTicketBorrowContinuousPublishesBothAndSourceHoldDoesNotCancelValidation(t *testing.T) {
	s, base := borrowFixture()
	base.route = nil
	r := &continuousTestRepo{replenishmentWorkerRepo: &replenishmentWorkerRepo{prefetchReleaseRepo: &prefetchReleaseRepo{observationWorkerRepo: &observationWorkerRepo{borrowWorkerRepo: &borrowWorkerRepo{borrowTestRepo: base}}}, state: &BorrowReplenishmentState{SourceID: 3, Until: time.Now().Add(60 * time.Millisecond), Attempts: 1, IntervalSeconds: 10}}}
	s.repo = r
	s.owner.httpUpstream = continuousTestUpstream{}
	s.owner.openaiGatewayService = &OpenAIGatewayService{accountRepo: s.owner.accountRepo}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	s.runTarget(ctx, BorrowTarget{AccountID: 11, Revision: 2, ProbePolicy: BorrowProbePolicy{Concurrency: 2, RenewBeforeSeconds: 15}}, BorrowSettings{Running: true, TTLSeconds: 230, Replenishment: &BorrowReplenishmentSettings{Enabled: true, Inventory: 2, IntervalSeconds: 10, HoldSeconds: 150}}, "continuous")
	require.NoError(t, ctx.Err())
	require.Len(t, r.published, 2)
	require.NotEqual(t, r.published[0].CookieHash, r.published[1].CookieHash)
	require.Equal(t, []string{"target_probe_passed", "target_probe_passed"}, r.outcomes)
}
