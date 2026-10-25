package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAstraReadinessRequiresCurrentTargetPass(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.CodexGatewayPin = pinConfig()
	wrapper := &astraRoutingUpstream{cfg: cfg}
	pool := wrapper.current(t.Context())
	now := time.Now()
	route := codexGatewayRouteFromResponse(pinResponse("__oailb=first; Path=/; Secure; Max-Age=3600"), "/backend-api/codex/responses", now)
	pool.recordSource(299, now, route, true)
	snapshot := wrapper.AstraGatewaySnapshot(t.Context())
	require.Zero(t, snapshot.ReadyRoutes)
	require.Equal(t, "candidate", snapshot.Sources[0].State)
	require.Nil(t, snapshot.Sources[0].ExpiresAt)
	require.Zero(t, snapshot.Sources[0].RemainingSeconds)
	pool.targetChecks = map[int64]astraTargetValidation{300: {cookieFingerprint: sha256.Sum256([]byte("first")), sourceID: 299, passed: false, checked: now, expires: route.expires, reason: "target_probe_degraded"}}
	snapshot = wrapper.AstraGatewaySnapshot(t.Context())
	require.Zero(t, snapshot.ReadyRoutes)
	require.Equal(t, "source_passed_target_failed", snapshot.Sources[0].Reason)
	require.Nil(t, snapshot.Sources[0].ExpiresAt)
	require.Equal(t, "target_probe_degraded", snapshot.Targets[0].Reason)
	require.Nil(t, snapshot.Targets[0].ExpiresAt)
	require.Zero(t, snapshot.Targets[0].RemainingSeconds)
	check := pool.targetChecks[300]
	check.passed = true
	pool.targetChecks[300] = check
	snapshot = wrapper.AstraGatewaySnapshot(t.Context())
	require.Equal(t, 1, snapshot.ReadyRoutes)
	require.NotNil(t, snapshot.Sources[0].ExpiresAt)
	require.NotNil(t, snapshot.Targets[0].ExpiresAt)
	// A newer cookie on the same gateway/source must not inherit an older pass.
	next := *route
	next.cookie.Value = "second"
	pool.recordSource(299, now.Add(time.Second), &next, true)
	snapshot = wrapper.AstraGatewaySnapshot(t.Context())
	require.Zero(t, snapshot.ReadyRoutes)
	require.Nil(t, snapshot.Sources[0].ExpiresAt)
	require.Nil(t, snapshot.Targets[0].ExpiresAt)
}
func TestAstraReadinessSnapshotDoesNotWaitForNetwork(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.CodexGatewayPin = pinConfig()
	wrapper := &astraRoutingUpstream{cfg: cfg}
	pool := wrapper.current(t.Context())
	require.NoError(t, pool.targetProbeMu.Lock(t.Context()))
	defer pool.targetProbeMu.Unlock()
	done := make(chan struct{})
	go func() { wrapper.AstraGatewaySnapshot(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("status blocked on target network probe")
	}
}

func TestAstraSchedulingToggleRetainsPool(t *testing.T) {
	settings := config.AstraRoutingSettings{Revision: "one", CookiePool: pinConfig()}
	cfg := &config.Config{}
	cfg.SetAstraRoutingLoader(func(context.Context) config.AstraRoutingSettings { return settings })
	wrapper := &astraRoutingUpstream{cfg: cfg}
	pool := wrapper.current(t.Context())
	settings.AccountScheduling = true
	require.Same(t, pool, wrapper.current(t.Context()))
	settings.SchedulingMode = "groups"
	settings.SchedulingGroupIDs = []int64{7}
	require.Same(t, pool, wrapper.current(t.Context()))
	settings.CookiePool.SourceAccountModels = map[int64]string{299: "gpt-5.6-sol"}
	next := wrapper.current(t.Context())
	require.NotSame(t, pool, next, "source model changes invalidate old routes and target passes")
	require.Empty(t, next.routes)
	require.Empty(t, next.targetChecks)
	settings.Revision = "two"
	require.NotSame(t, pool, wrapper.current(t.Context()))
}

func TestAstraProbeTransportFailureRevokesExistingPass(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.CodexGatewayPin = pinConfig()
	fail := false
	wrapper := &astraRoutingUpstream{cfg: cfg}
	wrapper.delegate = gatewayPinDelegate{call: func(_ *http.Request, _ string, id int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		if id == 299 {
			return pinResponse("__oailb=route; Path=/; Secure; Max-Age=230"), nil
		}
		if fail {
			return nil, errors.New("offline")
		}
		return pinResponse(""), nil
	}}
	resp, err := wrapper.Do(pinRequest(t), "", 299, 1)
	consumeAffinityResponse(t, resp, err)
	resp, err = wrapper.Do(forceAstraProbe(pinRequest(t)), "", 300, 1)
	consumeAffinityResponse(t, resp, err)
	require.Equal(t, 1, wrapper.AstraGatewaySnapshot(t.Context()).ReadyRoutes)
	fail = true
	req := pinRequest(t)
	err = wrapper.VerifyAstraGatewayTarget(t.Context(), req, "", 300, 1)
	require.Error(t, err)
	require.Zero(t, wrapper.AstraGatewaySnapshot(t.Context()).ReadyRoutes)
}

func TestAstraStateProbeBypassesBorrowingRecursion(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.CodexGatewayPin = pinConfig()
	calls := 0
	wrapper := &astraRoutingUpstream{cfg: cfg, delegate: gatewayPinDelegate{call: func(r *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		calls++
		require.True(t, service.IsOpenAICodexStateProbeRequest(r.Context()))
		return pinResponse(""), nil
	}}}
	req := pinRequest(t)
	req.Header.Set("Cookie", "__oailb=explicit-route")
	result := service.ProbeOpenAICodexStateRoute(t.Context(), wrapper, req, "exit", 300, 1, nil)
	require.Equal(t, service.OpenAICodexStateHealthy, result.Verdict)
	require.Equal(t, 2, calls)
	require.Nil(t, wrapper.pool, "standalone probes must not acquire or publish borrowing routes")
}

func TestAstraSourcesAcquireConcurrentlyAndKeepBackup(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.CodexGatewayPin = pinConfig()
	wrapper := &astraRoutingUpstream{cfg: cfg}
	entered := make(chan int64, 2)
	release := make(chan struct{})
	wrapper.SetAstraGatewayPreparer(func(ctx context.Context, id int64) error {
		entered <- id
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		now := time.Now()
		wrapper.current(ctx).recordSource(id, now, &codexGatewayRoute{cookie: http.Cookie{Name: "__oailb", Value: fmt.Sprint(id), Path: "/"}, expires: now.Add(230 * time.Second)}, true)
		return nil
	})
	done := make(chan error, 1)
	go func() { done <- wrapper.PrepareAstraGateway(t.Context()) }()
	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("sources did not acquire concurrently")
		}
	}
	close(release)
	require.NoError(t, <-done)
	require.Len(t, wrapper.current(t.Context()).routes, 2, "first success must not cancel backup acquisition")
}

func TestAstraTargetRejectsDonorAndUsesIndependentBackup(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.CodexGatewayPin = pinConfig()
	wrapper := &astraRoutingUpstream{cfg: cfg}
	pool := wrapper.current(t.Context())
	now := time.Now()
	for _, id := range []int64{299, 298} {
		pool.recordSource(id, now, &codexGatewayRoute{cookie: http.Cookie{Name: "__oailb", Value: fmt.Sprint(id), Path: "/"}, expires: now.Add(230 * time.Second)}, true)
	}
	var cookies []string
	wrapper.delegate = gatewayPinDelegate{call: func(r *http.Request, _ string, id int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		require.Equal(t, int64(300), id)
		require.Equal(t, "Bearer target-only", r.Header.Get("Authorization"))
		cookie, err := r.Cookie("__oailb")
		require.NoError(t, err)
		cookies = append(cookies, cookie.Value)
		resp := pinResponse("")
		if cookie.Value == "299" && r.Header.Get("X-Codex-Turn-State") != "" {
			resp.Header.Set("X-Codex-Turn-State", "replacement-ticket")
		}
		return resp, nil
	}}
	resp, err := wrapper.Do(rotationTarget(t), "", 300, 1)
	consumeAffinityResponse(t, resp, err)
	require.Equal(t, []string{"299", "299", "298", "298", "298"}, cookies)
	require.Equal(t, int64(298), pool.activeSource)
	require.NotContains(t, pool.routes, int64(299))
	// Replaying the rejected Cookie under another source must not revive it.
	cfg.Gateway.CodexGatewayPin.SourceAccountIDs = append(cfg.Gateway.CodexGatewayPin.SourceAccountIDs, 297)
	pool.config.SourceAccountIDs = append(pool.config.SourceAccountIDs, 297)
	pool.recordSource(297, now.Add(time.Second), &codexGatewayRoute{cookie: http.Cookie{Name: "__oailb", Value: "299", Path: "/"}, expires: now.Add(230 * time.Second)}, true)
	pool.mu.Lock()
	delete(pool.routes, 298)
	pool.mu.Unlock()
	cookie, _ := pool.currentCookie("/backend-api/codex/responses", time.Now())
	require.Nil(t, cookie)
	require.Equal(t, "rejected", pool.statuses[297].State)
}

func TestAstraTargetAutomaticallyReacquiresAfterCandidatesFail(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		freshPass, replay            bool
		wantAcquisitions, wantProbes int
		wantError                    string
	}{
		{name: "fresh Cookie recovers", freshPass: true, wantAcquisitions: 1, wantProbes: 4},
		{name: "rejected Cookie cannot revive", replay: true, wantAcquisitions: 1, wantProbes: 2, wantError: "no_qualified_source_route"},
		{name: "fresh failures are bounded", wantAcquisitions: 2, wantProbes: 6, wantError: "target_probe_degraded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Gateway.CodexGatewayPin = pinConfig()
			cfg.Gateway.CodexGatewayPin.SourceAccountIDs = []int64{299}
			wrapper := &astraRoutingUpstream{cfg: cfg}
			pool := wrapper.current(t.Context())
			now := time.Now()
			pool.recordSource(299, now, &codexGatewayRoute{cookie: http.Cookie{Name: "__oailb", Value: "initial", Path: "/"}, expires: now.Add(230 * time.Second)}, true)
			acquisitions := 0
			wrapper.SetAstraGatewayPreparer(func(ctx context.Context, id int64) error {
				acquisitions++
				require.Equal(t, int64(299), id)
				value := fmt.Sprintf("fresh-%d", acquisitions)
				if tc.replay {
					value = "initial"
				}
				p := wrapper.current(ctx)
				p.mu.Lock()
				require.Equal(t, "reacquiring_after_target_failure", p.statuses[id].Reason)
				p.mu.Unlock()
				p.recordSource(id, time.Now(), &codexGatewayRoute{cookie: http.Cookie{Name: "__oailb", Value: value, Path: "/"}, expires: now.Add(230 * time.Second)}, true)
				return nil
			})
			probes := 0
			wrapper.delegate = gatewayPinDelegate{call: func(r *http.Request, _ string, id int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
				probes++
				require.Equal(t, int64(300), id)
				require.Equal(t, "Bearer target-only", r.Header.Get("Authorization"))
				cookie, err := r.Cookie("__oailb")
				require.NoError(t, err)
				resp := pinResponse("")
				if (!tc.freshPass || cookie.Value == "initial") && r.Header.Get("X-Codex-Turn-State") != "" {
					resp.Header.Set("X-Codex-Turn-State", "replacement-ticket")
				}
				return resp, nil
			}}
			err := wrapper.VerifyAstraGatewayTarget(t.Context(), rotationTarget(t), "", 300, 1)
			if tc.wantError == "" {
				require.NoError(t, err)
				require.Equal(t, 1, wrapper.AstraGatewaySnapshot(t.Context()).ReadyRoutes)
			} else {
				require.ErrorContains(t, err, tc.wantError)
				require.Zero(t, wrapper.AstraGatewaySnapshot(t.Context()).ReadyRoutes)
			}
			require.Equal(t, tc.wantAcquisitions, acquisitions)
			require.Equal(t, tc.wantProbes, probes)
		})
	}
}

func TestAstraAutomaticReacquisitionSerializesAndCancels(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.CodexGatewayPin = pinConfig()
	cfg.Gateway.CodexGatewayPin.SourceAccountIDs = []int64{299}
	wrapper := &astraRoutingUpstream{cfg: cfg}
	pool := wrapper.current(t.Context())
	now := time.Now()
	pool.recordSource(299, now, &codexGatewayRoute{cookie: http.Cookie{Name: "__oailb", Value: "initial", Path: "/"}, expires: now.Add(230 * time.Second)}, true)
	wrapper.delegate = gatewayPinDelegate{call: func(r *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		resp := pinResponse("")
		if r.Header.Get("X-Codex-Turn-State") != "" {
			resp.Header.Set("X-Codex-Turn-State", "replacement-ticket")
		}
		return resp, nil
	}}
	entered := make(chan struct{})
	wrapper.SetAstraGatewayPreparer(func(ctx context.Context, _ int64) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- wrapper.VerifyAstraGatewayTarget(ctx, rotationTarget(t), "", 300, 1) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("automatic reacquisition did not start")
	}
	require.ErrorContains(t, wrapper.VerifyAstraGatewayTarget(t.Context(), rotationTarget(t), "", 300, 1), "target_validation_in_progress")
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.Zero(t, wrapper.AstraGatewaySnapshot(t.Context()).ReadyRoutes)
	require.True(t, pool.targetRecoveryMu.TryLock(), "cancellation must release recovery serialization")
	pool.targetRecoveryMu.Unlock()
}

func TestAstraReacquiresBeforeLargeCandidateBatchExhaustsTimeBudget(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.CodexGatewayPin = pinConfig()
	cfg.Gateway.CodexGatewayPin.SourceAccountIDs = []int64{299, 298, 297, 296}
	wrapper := &astraRoutingUpstream{cfg: cfg}
	pool := wrapper.current(t.Context())
	now := time.Now()
	for _, id := range cfg.Gateway.CodexGatewayPin.SourceAccountIDs {
		pool.recordSource(id, now, &codexGatewayRoute{cookie: http.Cookie{Name: "__oailb", Value: fmt.Sprintf("old-%d", id), Path: "/"}, expires: now.Add(230 * time.Second)}, true)
	}
	acquisitions := make(chan int64, 4)
	wrapper.SetAstraGatewayPreparer(func(ctx context.Context, id int64) error {
		acquisitions <- id
		wrapper.current(ctx).recordSource(id, time.Now(), &codexGatewayRoute{cookie: http.Cookie{Name: "__oailb", Value: fmt.Sprintf("fresh-%d", id), Path: "/"}, expires: now.Add(230 * time.Second)}, true)
		return nil
	})
	var probes []string
	wrapper.delegate = gatewayPinDelegate{call: func(r *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		cookie, err := r.Cookie("__oailb")
		require.NoError(t, err)
		probes = append(probes, cookie.Value)
		resp := pinResponse("")
		if cookie.Value != "fresh-296" && r.Header.Get("X-Codex-Turn-State") != "" {
			resp.Header.Set("X-Codex-Turn-State", "replacement-ticket")
		}
		return resp, nil
	}}
	require.NoError(t, wrapper.VerifyAstraGatewayTarget(t.Context(), rotationTarget(t), "", 300, 1))
	require.Len(t, acquisitions, 4, "three failures must trigger fresh acquisition even with a larger source list")
	require.Equal(t, []string{"old-299", "old-299", "old-298", "old-298", "old-297", "old-297", "fresh-296", "fresh-296"}, probes)
}

func TestAstraVerifiedRouteAllowsFiveConcurrentBusinessRequests(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.CodexGatewayPin = pinConfig()
	wrapper := &astraRoutingUpstream{cfg: cfg}
	pool := wrapper.current(t.Context())
	now := time.Now()
	pool.recordSource(299, now, &codexGatewayRoute{cookie: http.Cookie{Name: "__oailb", Value: "verified", Path: "/"}, expires: now.Add(230 * time.Second)}, true)
	entered := make(chan struct{}, 5)
	resume := make(chan struct{})
	wrapper.delegate = gatewayPinDelegate{call: func(r *http.Request, _ string, id int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		require.Equal(t, int64(300), id)
		require.Equal(t, "Bearer target-only", r.Header.Get("Authorization"))
		if !service.IsOpenAICodexStateProbeRequest(r.Context()) {
			entered <- struct{}{}
			<-resume
		}
		return pinResponse(""), nil
	}}
	require.NoError(t, wrapper.VerifyAstraGatewayTarget(t.Context(), rotationTarget(t), "", 300, 1))
	// Even while a different validation owns the locks, exact existing passes
	// may proceed; an identity mismatch must never start another probe.
	pool.targetRecoveryMu.Lock()
	require.NoError(t, pool.targetProbeMu.Lock(t.Context()))
	done := make(chan error, 5)
	for range 5 {
		go func() {
			resp, err := wrapper.Do(rotationTarget(t), "", 300, 1)
			if err == nil {
				_, err = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
			done <- err
		}()
	}
	for range 5 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			close(resume)
			pool.targetProbeMu.Unlock()
			pool.targetRecoveryMu.Unlock()
			t.Fatal("cached route blocked concurrent business requests")
		}
	}
	other := rotationTarget(t)
	other.Header.Set("Authorization", "Bearer other-identity")
	_, err := wrapper.Do(other, "", 300, 1)
	require.ErrorContains(t, err, "target_validation_in_progress")
	close(resume)
	for range 5 {
		require.NoError(t, <-done)
	}
	pool.targetProbeMu.Unlock()
	pool.targetRecoveryMu.Unlock()
}
