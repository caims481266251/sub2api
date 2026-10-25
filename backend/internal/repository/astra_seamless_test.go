package repository

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func seamlessFixture(t *testing.T) (*astraRoutingUpstream, *codexGatewayPinUpstream, *astraSeamlessIdentity) {
	t.Helper()
	cfg := &config.Config{}
	cfg.Gateway.CodexGatewayPin = pinConfig()
	cfg.Gateway.CodexGatewayPin.Seamless = true
	s := &astraRoutingUpstream{cfg: cfg, delegate: gatewayPinDelegate{call: func(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error) {
		return pinResponse(""), nil
	}}}
	pool := s.current(t.Context())
	t.Cleanup(s.StopAstraSeamless)
	entry, err := pool.seamless.identity(rotationBusinessTarget(t), "exit", 300, 1, nil, true, time.Now())
	require.NoError(t, err)
	return s, pool, entry
}
func seamlessRecord(p *astraSeamlessPool, source int64, value string, expiry time.Time) {
	p.record(source, codexGatewayRoute{cookie: http.Cookie{Name: "__oailb", Value: value, Path: "/"}, expires: expiry}, time.Now())
}
func seamlessWait(t *testing.T, done <-chan struct{}) {
	t.Helper()
	require.NotNil(t, done)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("maintenance did not finish")
	}
}

func TestSeamlessKeepsPrimaryWhenSourceChangesAndBackupFails(t *testing.T) {
	s, pool, e := seamlessFixture(t)
	p := pool.seamless
	expiry := time.Now().Add(230 * time.Second)
	seamlessRecord(p, 299, "primary", expiry)
	require.NoError(t, s.fillSeamless(t.Context(), pool, e, true))
	s.delegate = gatewayPinDelegate{call: func(r *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		c, _ := r.Cookie("__oailb")
		if c.Value == "primary" {
			return pinResponse(""), nil
		}
		return &http.Response{StatusCode: 403, Header: http.Header{}, Body: http.NoBody}, nil
	}}
	seamlessRecord(p, 299, "replacement", expiry.Add(time.Second))
	require.Error(t, s.fillSeamless(t.Context(), pool, e, false))
	require.Len(t, e.passes, 1)
	require.Equal(t, "primary", e.passes[0].candidate.route.cookie.Value)
	cookie, _, _, release, err := s.seamlessTargetRoute(rotationBusinessTarget(t), "exit", 300, 1, nil, pool)
	defer release()
	require.NoError(t, err)
	require.Equal(t, "primary", cookie.Value)
	require.Equal(t, "ready", s.AstraGatewaySnapshot(t.Context()).Targets[0].State)
}

func TestSeamlessIndependentTargetRejectionAndInventory(t *testing.T) {
	s, pool, e := seamlessFixture(t)
	p := pool.seamless
	seamlessRecord(p, 299, "route", time.Now().Add(230*time.Second))
	require.NoError(t, s.fillSeamless(t.Context(), pool, e, true))
	other, err := p.identity(rotationBusinessTarget(t), "exit", 301, 1, nil, true, time.Now())
	require.NoError(t, err)
	s.delegate = gatewayPinDelegate{call: func(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Header: http.Header{}, Body: http.NoBody}, nil
	}}
	require.Error(t, s.fillSeamless(t.Context(), pool, other, true))
	require.Len(t, e.passes, 1)
	require.Empty(t, other.passes)
	require.Len(t, p.candidates, 1)
}

func TestSeamlessDuplicateDoesNotRenewOrCountTwice(t *testing.T) {
	s, pool, e := seamlessFixture(t)
	p := pool.seamless
	expiry := time.Now().Add(80 * time.Second)
	seamlessRecord(p, 299, "same", expiry)
	seamlessRecord(p, 298, "same", expiry.Add(100*time.Second))
	require.Len(t, p.candidates, 1)
	for _, c := range p.candidates {
		require.Equal(t, expiry, c.route.expires)
	}
	require.NoError(t, s.fillSeamless(t.Context(), pool, e, true))
	require.Len(t, e.passes, 1)
	// Expired samples cannot be resurrected by reacquiring the same Cookie.
	p.mu.Lock()
	for key := range p.deadlines {
		p.deadlines[key] = time.Now().Add(-time.Second)
	}
	p.candidates = map[[32]byte]astraSeamlessCandidate{}
	p.mu.Unlock()
	seamlessRecord(p, 299, "same", time.Now().Add(230*time.Second))
	require.Empty(t, p.candidates)
}

func TestSeamlessPrewarmUsesFreshestAndSwitchesBeforeExpiry(t *testing.T) {
	s, pool, e := seamlessFixture(t)
	p := pool.seamless
	now := time.Now()
	seamlessRecord(p, 299, "old", now.Add(100*time.Second))
	require.NoError(t, s.fillSeamless(t.Context(), pool, e, true))
	seamlessRecord(p, 298, "new", now.Add(220*time.Second))
	require.NoError(t, s.fillSeamless(t.Context(), pool, e, false))
	require.Len(t, e.passes, 2)
	require.False(t, seamlessNeedsFill(e, now))
	require.Nil(t, s.startSeamlessJob(pool, e, false))
	p.mu.Lock()
	e.passes[0].candidate.route.expires = now.Add(10 * time.Second)
	p.mu.Unlock()
	cookie, _, _, release, err := s.seamlessTargetRoute(rotationBusinessTarget(t), "exit", 300, 1, nil, pool)
	release()
	require.NoError(t, err)
	require.Equal(t, "new", cookie.Value)
}

func TestSeamlessCredentialsAndProfileNeverReuseUnmatchedPass(t *testing.T) {
	s, pool, e := seamlessFixture(t)
	p := pool.seamless
	seamlessRecord(p, 299, "route", time.Now().Add(230*time.Second))
	require.NoError(t, s.fillSeamless(t.Context(), pool, e, true))
	req := rotationBusinessTarget(t)
	req.Header.Set("Version", "different")
	other, err := p.identity(req, "exit", 300, 1, nil, false, time.Now())
	require.NoError(t, err)
	require.Empty(t, other.passes)
	req.Header.Set("Authorization", "Bearer refreshed")
	_, err = p.identity(req, "exit", 300, 1, nil, false, time.Now())
	require.ErrorContains(t, err, "identity_changed")
	fresh, err := p.identity(req, "exit", 300, 1, nil, true, time.Now())
	require.NoError(t, err)
	require.Empty(t, fresh.passes)
	require.Len(t, p.identities[300], 1)
	_, err = p.identity(rotationBusinessTarget(t), "exit", 300, 1, nil, false, time.Now())
	require.ErrorContains(t, err, "identity_changed")
}

func TestSeamlessBackgroundSurvivesWaiterCancelAndPublishesNoStaleResults(t *testing.T) {
	s, pool, e := seamlessFixture(t)
	p := pool.seamless
	seamlessRecord(p, 299, "candidate", time.Now().Add(230*time.Second))
	entered := make(chan struct{}, 1)
	resume := make(chan struct{})
	s.delegate = gatewayPinDelegate{call: func(r *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-resume:
			return pinResponse(""), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}}
	done := s.startSeamlessJob(pool, e, true)
	<-entered
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req := rotationBusinessTarget(t).Clone(ctx)
	_, _, _, release, err := s.seamlessTargetRoute(req, "exit", 300, 1, nil, pool)
	release()
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, p.ctx.Err())
	// A credential generation change cancels the old worker and rejects its result.
	freshReq := rotationBusinessTarget(t)
	freshReq.Header.Set("Authorization", "Bearer new")
	fresh, err := p.identity(freshReq, "exit", 300, 1, nil, true, time.Now())
	require.NoError(t, err)
	close(resume)
	seamlessWait(t, done)
	require.Empty(t, fresh.passes)
}

func TestSeamlessTenBusinessRequestsContinueDuringBackupProbe(t *testing.T) {
	s, pool, e := seamlessFixture(t)
	p := pool.seamless
	seamlessRecord(p, 299, "primary", time.Now().Add(230*time.Second))
	require.NoError(t, s.fillSeamless(t.Context(), pool, e, true))
	seamlessRecord(p, 298, "backup", time.Now().Add(231*time.Second))
	entered := make(chan struct{}, 1)
	resume := make(chan struct{})
	var calls atomic.Int32
	s.delegate = gatewayPinDelegate{call: func(r *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		if service.IsOpenAICodexStateProbeRequest(r.Context()) {
			select {
			case entered <- struct{}{}:
			default:
			}
			select {
			case <-resume:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		} else {
			calls.Add(1)
		}
		return pinResponse(""), nil
	}}
	done := s.startSeamlessJob(pool, e, false)
	<-entered
	results := make(chan error, 10)
	for range 10 {
		go func() {
			resp, err := s.Do(rotationBusinessTarget(t), "exit", 300, 1)
			if resp != nil {
				_ = resp.Body.Close()
			}
			results <- err
		}()
	}
	for range 10 {
		select {
		case err := <-results:
			require.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatal("business blocked on backup")
		}
	}
	require.EqualValues(t, 10, calls.Load())
	close(resume)
	seamlessWait(t, done)
}

func TestSeamlessBudgetsAndBoundedCandidateStorage(t *testing.T) {
	p := newAstraSeamlessPool()
	defer p.cancel()
	now := time.Now()
	for range 4 {
		require.NoError(t, p.consume(true, 299, now))
	}
	require.Error(t, p.consume(true, 299, now))
	require.NoError(t, p.consume(true, 299, now.Add(time.Minute)))
	for i := range 40 {
		seamlessRecord(p, int64(i+1), fmt.Sprint(i), now.Add(230*time.Second))
	}
	require.Len(t, p.candidates, 32)
	for range 24 {
		require.NoError(t, p.consume(false, 300, now.Add(2*time.Minute)))
	}
	require.Error(t, p.consume(false, 300, now.Add(2*time.Minute)))
}

func TestSeamlessSourceAcquisitionStartsOnlyOneSmallBatch(t *testing.T) {
	s, pool, _ := seamlessFixture(t)
	pool.config.SourceAccountIDs = []int64{1, 2, 3, 4, 5}
	var calls atomic.Int32
	s.SetAstraGatewayPreparer(func(context.Context, int64) error { calls.Add(1); return errors.New("offline") })
	require.Error(t, s.PrepareAstraGateway(t.Context()))
	require.EqualValues(t, 2, calls.Load())
}

func TestSeamlessFourRenewalWindowsKeepBusinessReady(t *testing.T) {
	s, pool, e := seamlessFixture(t)
	p := pool.seamless
	for cycle := range 4 {
		now := time.Now()
		seamlessRecord(p, 299, fmt.Sprintf("generation-%d-a", cycle), now.Add(230*time.Second))
		seamlessRecord(p, 298, fmt.Sprintf("generation-%d-b", cycle), now.Add(231*time.Second))
		require.NoError(t, s.fillSeamless(t.Context(), pool, e, false))
		require.Len(t, e.passes, 2)
		// The old primary reaches the admission margin while the backup stays valid.
		p.mu.Lock()
		e.passes[0].candidate.route.expires = now.Add(10 * time.Second)
		p.mu.Unlock()
		cookie, _, _, release, err := s.seamlessTargetRoute(rotationBusinessTarget(t), "exit", 300, 1, nil, pool)
		release()
		require.NoError(t, err)
		require.NotEmpty(t, cookie.Value)
		p.mu.Lock()
		for i := range e.passes {
			e.passes[i].candidate.route.expires = now.Add(100 * time.Second)
		}
		// Do not reuse older candidate samples in the next renewal window.
		p.candidates = map[[32]byte]astraSeamlessCandidate{}
		p.mu.Unlock()
	}
}

func TestSeamlessBusinessRouteChangeRevokesOnlyMatchingPass(t *testing.T) {
	s, pool, e := seamlessFixture(t)
	p := pool.seamless
	seamlessRecord(p, 299, "primary", time.Now().Add(230*time.Second))
	require.NoError(t, s.fillSeamless(t.Context(), pool, e, true))
	seamlessRecord(p, 298, "backup", time.Now().Add(231*time.Second))
	require.NoError(t, s.fillSeamless(t.Context(), pool, e, false))
	resp := pinResponse("__oailb=changed; Path=/; Secure")
	defer resp.Body.Close()
	p.observeBusiness(rotationBusinessTarget(t), "exit", 300, nil, &http.Cookie{Name: "__oailb", Value: "primary"}, resp)
	require.Len(t, e.passes, 1)
	require.Equal(t, "backup", e.passes[0].candidate.route.cookie.Value)
}

func TestSeamlessInFlightProbeCannotUndoBusinessRevocation(t *testing.T) {
	s, pool, e := seamlessFixture(t)
	p := pool.seamless
	seamlessRecord(p, 299, "primary", time.Now().Add(230*time.Second))
	require.NoError(t, s.fillSeamless(t.Context(), pool, e, true))
	entered := make(chan struct{}, 1)
	resume := make(chan struct{})
	s.delegate = gatewayPinDelegate{call: func(r *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-resume:
			return pinResponse(""), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}}
	done := s.startSeamlessJob(pool, e, true)
	<-entered
	p.observeBusiness(rotationBusinessTarget(t), "exit", 300, nil, &http.Cookie{Value: "primary"}, &http.Response{StatusCode: 401, Header: http.Header{}})
	close(resume)
	seamlessWait(t, done)
	require.Empty(t, e.passes)
}
func TestSeamlessUpstreamRetryAfterPreventsAdditionalSends(t *testing.T) {
	p := newAstraSeamlessPool()
	defer p.cancel()
	var calls atomic.Int32
	up := astraSeamlessUpstream{HTTPUpstream: gatewayPinDelegate{call: func(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"120"}}, Body: http.NoBody}, nil
	}}, p: p}
	resp, err := up.Do(rotationBusinessTarget(t), "exit", 300, 1)
	require.NoError(t, err)
	_ = resp.Body.Close()
	_, err = up.Do(rotationBusinessTarget(t), "exit", 300, 1)
	require.Error(t, err)
	require.EqualValues(t, 1, calls.Load())
	require.True(t, p.cooling[300].After(time.Now().Add(119*time.Second)))
}

func TestSeamlessFullInventorySkipsCandidatesThatDoNotExtendCoverage(t *testing.T) {
	s, pool, e := seamlessFixture(t)
	p := pool.seamless
	now := time.Now()
	seamlessRecord(p, 299, "primary", now.Add(100*time.Second))
	require.NoError(t, s.fillSeamless(t.Context(), pool, e, true))
	seamlessRecord(p, 298, "backup", now.Add(140*time.Second))
	require.NoError(t, s.fillSeamless(t.Context(), pool, e, true))
	// Force checks above only revalidate primary; explicitly seed the second pass.
	p.mu.Lock()
	for _, c := range p.candidates {
		if c.route.cookie.Value == "backup" {
			e.passes = append(e.passes, astraSeamlessPass{c, now})
		}
	}
	p.mu.Unlock()
	seamlessRecord(p, 298, "old-unused", now.Add(130*time.Second))
	var probes atomic.Int32
	s.delegate = gatewayPinDelegate{call: func(r *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		probes.Add(1)
		c, _ := r.Cookie("__oailb")
		require.Equal(t, "fresh", c.Value)
		return pinResponse(""), nil
	}}
	s.SetAstraGatewayPreparer(func(context.Context, int64) error {
		seamlessRecord(p, 298, "fresh", time.Now().Add(230*time.Second))
		return nil
	})
	require.NoError(t, s.fillSeamless(t.Context(), pool, e, false))
	require.EqualValues(t, 2, probes.Load())
}
