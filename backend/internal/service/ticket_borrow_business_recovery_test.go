package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func recoveryFixture(t *testing.T) (context.Context, *BorrowBusinessRecovery) {
	t.Helper()
	ctx, r := WithBorrowBusinessRecovery(t.Context(), nil)
	t.Cleanup(r.Close)
	require.NoError(t, r.Activate(11, false))
	require.NoError(t, r.BeginTransport(ctx))
	return ctx, r
}

func TestBorrowBusinessRecoverySharedBudgetAnd429(t *testing.T) {
	t.Run("one recovery across targets and detached contexts", func(t *testing.T) {
		ctx, r := recoveryFixture(t)
		r.WroteHeaders(ctx)
		r.ExplicitFailure()
		require.True(t, r.ReserveRecovery(ctx, 503))
		require.NoError(t, r.Activate(62, false))
		detached := context.WithoutCancel(ctx)
		require.Same(t, r, BorrowBusinessRecoveryFromContext(detached))
		require.NoError(t, r.BeginTransport(detached))
		r.WroteHeaders(ctx)
		r.ExplicitFailure()
		require.False(t, r.ReserveRecovery(ctx, 503))
		require.ErrorIs(t, r.BeginTransport(ctx), ErrBorrowRecoveryBudget)
		require.Equal(t, 2, r.sends)
	})
	t.Run("429 cannot use legacy deadline exception", func(t *testing.T) {
		ctx, r := recoveryFixture(t)
		r.WroteHeaders(ctx)
		r.ExplicitFailure()
		require.True(t, r.ReserveRecovery(ctx, http.StatusTooManyRequests))
		require.NoError(t, r.BeginTransport(ctx))
		r.WroteHeaders(ctx)
		r.ExplicitFailure()
		require.False(t, r.ReserveRecovery(ctx, http.StatusTooManyRequests))
		require.Equal(t, "attempt_limit", r.stopReason)
		require.ErrorIs(t, r.BeginTransport(ctx), ErrBorrowRecoveryBudget)
	})
	t.Run("late explicit failure retains one recovery", func(t *testing.T) {
		ctx, r := recoveryFixture(t)
		r.WroteHeaders(ctx)
		r.ExplicitFailure()
		r.firstSend = time.Now().Add(-179 * time.Second)
		require.True(t, r.ReserveRecovery(ctx, 503))
		require.NoError(t, r.BeginTransport(ctx))
		r.WroteHeaders(ctx)
		r.ExplicitFailure()
		require.False(t, r.ReserveRecovery(ctx, 503))
		require.Equal(t, "attempt_limit", r.stopReason)
	})
	t.Run("duplicate failure does not extend recovery initiation window", func(t *testing.T) {
		ctx, r := recoveryFixture(t)
		r.WroteHeaders(ctx)
		r.ExplicitFailure()
		r.failureAt = time.Now().Add(-16 * time.Second)
		r.ExplicitFailure()
		require.False(t, r.ReserveRecovery(ctx, 503))
		require.Equal(t, "retry_window_elapsed", r.stopReason)
	})
	t.Run("late pre-write failure still has a bounded window", func(t *testing.T) {
		ctx, r := recoveryFixture(t)
		r.firstStart = time.Now().Add(-16 * time.Second)
		require.False(t, r.ReserveRecovery(ctx, 502))
		require.Equal(t, "retry_window_elapsed", r.stopReason)
	})
}

func TestBorrowBusinessRecoveryReplaySafety(t *testing.T) {
	t.Run("unknown outcome after write is not replayed", func(t *testing.T) {
		ctx, r := recoveryFixture(t)
		r.WroteHeaders(ctx)
		require.False(t, r.ReserveRecovery(ctx, 502))
		require.Equal(t, "ambiguous_upstream_execution", r.stopReason)
	})
	t.Run("pre-write failure can recover once", func(t *testing.T) {
		ctx, r := recoveryFixture(t)
		require.True(t, r.MayRetryBeforeWrite())
		require.True(t, r.ReserveRecovery(ctx, 502))
		require.NoError(t, r.BeginTransport(ctx))
		require.ErrorIs(t, r.BeginTransport(ctx), ErrBorrowRecoveryBudget)
		require.Zero(t, r.sends)
	})
	t.Run("continuation cannot change account", func(t *testing.T) {
		_, r := recoveryFixture(t)
		require.NoError(t, r.Activate(11, true))
		require.ErrorIs(t, r.CheckTarget(62), ErrBorrowUnavailable)
		require.ErrorIs(t, r.Activate(62, false), ErrBorrowUnavailable)
	})
	t.Run("semantic output closes replay boundary", func(t *testing.T) {
		ctx, r := recoveryFixture(t)
		r.WroteHeaders(ctx)
		r.ExplicitFailure()
		r.OutputStarted(ctx)
		require.False(t, r.ReserveRecovery(ctx, 503))
	})
}

func TestBorrowBusinessRecoveryTimeoutAndDisarm(t *testing.T) {
	t.Run("deadline survives detached upstream", func(t *testing.T) {
		ctx, r := recoveryFixture(t)
		r.timeout = 20 * time.Millisecond
		r.ExplicitFailure()
		require.True(t, r.ReserveRecovery(ctx, 503))
		detached := r.Context(context.WithoutCancel(ctx))
		select {
		case <-detached.Done():
		case <-time.After(time.Second):
			t.Fatal("recovery did not expire")
		}
		require.ErrorIs(t, r.Err(), ErrBorrowRecoveryTimeout)
		require.ErrorIs(t, context.Cause(detached), ErrBorrowRecoveryTimeout)
	})
	t.Run("long successful stream is not canceled at recovery deadline", func(t *testing.T) {
		ctx, r := recoveryFixture(t)
		r.timeout = 20 * time.Millisecond
		r.ExplicitFailure()
		require.True(t, r.ReserveRecovery(ctx, 503))
		detached := r.Context(context.WithoutCancel(ctx))
		r.OutputStarted(ctx)
		select {
		case <-detached.Done():
			t.Fatal("successful stream canceled")
		case <-time.After(40 * time.Millisecond):
		}
		require.NoError(t, r.Err())
	})
	t.Run("client cancellation stops recovery before output", func(t *testing.T) {
		client, cancel := context.WithCancel(t.Context())
		defer cancel()
		ctx, r := WithBorrowBusinessRecovery(client, nil)
		defer r.Close()
		require.NoError(t, r.Activate(11, false))
		require.NoError(t, r.BeginTransport(ctx))
		r.ExplicitFailure()
		require.True(t, r.ReserveRecovery(ctx, 503))
		detached := r.Context(context.WithoutCancel(ctx))
		cancel()
		select {
		case <-detached.Done():
		case <-time.After(time.Second):
			t.Fatal("client cancellation lost")
		}
	})
}

func TestBorrowBusinessRecoveryScopeAndObserver(t *testing.T) {
	ctx, r := WithBorrowBusinessRecovery(t.Context(), nil)
	defer r.Close()
	for i := 0; i < 5; i++ {
		require.NoError(t, r.BeginTransport(ctx))
	}
	require.False(t, r.Active())
	require.Zero(t, r.transports)
	_, disabled := WithBorrowBusinessRecovery(ctx, &config.Config{Gateway: config.GatewayConfig{BorrowBusinessRecoveryDisabled: true}})
	require.Nil(t, disabled)

	ctx, r = recoveryFixture(t)
	r.ObserveSSE(ctx, []byte(`{"type":"response.output_item.added","item":{"type":"reasoning","summary":[]}}`), "")
	require.False(t, r.committed, "empty metadata is not model output")
	r.ObserveSSE(ctx, []byte(`{"type":"response.output_text.delta","delta":"hello"}`), "")
	require.True(t, r.committed)

	ctx, r = recoveryFixture(t)
	r.ObserveSSE(ctx, []byte(`{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error"}}}`), "")
	require.True(t, r.explicitFailure)
	require.False(t, r.committed)
}

func TestBorrowBusinessRecoveryPartialWriteAndSemanticEvents(t *testing.T) {
	t.Run("partial header writes are ambiguous", func(t *testing.T) {
		ctx, r := recoveryFixture(t)
		r.WritePossible()
		require.False(t, r.MayRetryBeforeWrite())
		require.False(t, r.ReserveRecovery(ctx, 502))
		require.Zero(t, r.sends, "complete headers were never observed")
	})
	for _, payload := range []string{
		`{"type":"ping"}`, `{"type":"response.created"}`,
		`{"choices":[{"delta":{"role":"assistant"}}]}`,
		`{"choices":[],"usage":{"total_tokens":3}}`,
	} {
		t.Run(payload, func(t *testing.T) {
			ctx, r := recoveryFixture(t)
			r.ObserveSSE(ctx, []byte(payload), "")
			require.False(t, r.committed)
		})
	}
	for _, payload := range []string{
		`{"type":"response.output_text.delta","delta":"answer"}`,
		`{"choices":[{"delta":{"content":"answer"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"thinking"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1"}]}}]}`,
	} {
		t.Run(payload, func(t *testing.T) {
			ctx, r := recoveryFixture(t)
			r.ObserveSSE(ctx, []byte(payload), "")
			require.True(t, r.committed)
			r.ExplicitFailure()
			require.False(t, r.ReserveRecovery(ctx, 503))
		})
	}
}

func TestBorrowBusinessRecoveryTenConcurrentRequests(t *testing.T) {
	// Separate logical requests may each recover once; a race on the same request
	// must never authorize two recoveries or a third physical transport.
	for i := 0; i < 10; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			ctx, r := recoveryFixture(t)
			r.WroteHeaders(ctx)
			r.ExplicitFailure()
			results := make(chan bool, 10)
			var wg sync.WaitGroup
			for j := 0; j < 10; j++ {
				wg.Add(1)
				go func() { defer wg.Done(); results <- r.ReserveRecovery(ctx, 503) }()
			}
			wg.Wait()
			close(results)
			allowed := 0
			for ok := range results {
				if ok {
					allowed++
				}
			}
			require.Equal(t, 1, allowed)
			require.NoError(t, r.BeginTransport(ctx))
			require.ErrorIs(t, r.BeginTransport(ctx), ErrBorrowRecoveryBudget)
			r.OutputStarted(ctx)
			require.NoError(t, r.Err())
		})
	}
}

func TestBorrowBusinessRecoveryTimerOutputRace(t *testing.T) {
	for i := 0; i < 30; i++ {
		ctx, r := recoveryFixture(t)
		r.timeout = time.Millisecond
		r.ExplicitFailure()
		require.True(t, r.ReserveRecovery(ctx, 503))
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); r.OutputStarted(ctx) }()
		go func() { defer wg.Done(); r.expire(ErrBorrowRecoveryTimeout) }()
		wg.Wait()
		r.mu.Lock()
		inconsistent := r.committed && r.err != nil
		r.mu.Unlock()
		require.False(t, inconsistent, "timeout and output must be mutually exclusive")
	}
}

func TestBorrowBusinessRecoveryRemainsHTTP(t *testing.T) {
	ctx, r := recoveryFixture(t)
	r.ExplicitFailure()
	require.True(t, r.ReserveRecovery(ctx, 503))
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil).WithContext(ctx)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	s := &OpenAIGatewayService{}
	decision := s.resolveOpenAIHTTPWSSSEDecision(c, &Account{}, nil, OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2})
	require.Equal(t, OpenAIUpstreamTransportHTTPSSE, decision.Transport)
	_, finish, err := s.prepareCodexWSAnchor(ctx, c, &Account{}, nil)
	require.NoError(t, err)
	require.Nil(t, finish)
}

func TestBorrowBusinessRecoverySentRequestOutlivesWaitBudget(t *testing.T) {
	ctx, r := recoveryFixture(t)
	r.timeout = 10 * time.Millisecond
	r.ExplicitFailure()
	require.True(t, r.ReserveRecovery(ctx, 429))
	wait := r.Context(context.WithoutCancel(ctx))
	require.NoError(t, r.BeginTransport(wait))
	r.WroteHeaders(wait)
	// No output or response headers yet. Sending alone removes the wait deadline.
	time.Sleep(30 * time.Millisecond)
	require.NoError(t, r.Err())
	require.NoError(t, wait.Err())
	r.expire(ErrBorrowRecoveryTimeout) // even an already queued callback is harmless
	require.NoError(t, wait.Err())
}
func TestBorrowBusinessRouteWait(t *testing.T) {
	ctx, r := WithBorrowBusinessRecovery(t.Context(), nil)
	defer r.Close()
	s := &TicketBorrowService{}
	signal := s.routePublished()
	s.notifyRoutePublished()
	require.True(t, r.waitForRoute(ctx, signal), "publication before waiting must not be lost")
	deadline := r.routeWaitDeadline
	require.InDelta(t, r.timeout.Seconds(), time.Until(deadline).Seconds(), 0.2)
	require.True(t, r.waitForRoute(ctx, signal))
	require.Equal(t, deadline, r.routeWaitDeadline)
	r.routeWaitDeadline = time.Now().Add(10 * time.Millisecond)
	require.False(t, r.waitForRoute(ctx, s.routePublished()))
	require.False(t, r.waitForRoute(ctx, signal), "deadline is shared across reselections")
	ctx2, cancel := context.WithCancel(ctx)
	cancel()
	require.False(t, r.waitForRoute(ctx2, signal))
}

func TestBorrowBusinessRouteWaitUsesRequestBudget(t *testing.T) {
	client, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	ctx, r := WithBorrowBusinessRecovery(client, nil)
	defer r.Close()
	s := &TicketBorrowService{}
	signal := s.routePublished()
	s.notifyRoutePublished()
	require.True(t, r.waitForRoute(context.WithoutCancel(ctx), signal))
	deadline, _ := client.Deadline()
	require.Equal(t, deadline, r.routeWaitDeadline, "client deadline clamps the shared wait even after detaching")
	require.Greater(t, time.Until(r.routeWaitDeadline), 5*time.Second)
	require.Zero(t, r.transports)
	require.Zero(t, r.sends)
	require.True(t, r.failureAt.IsZero(), "admission wait does not begin the upstream-failure recovery window")
	cancel()
	require.False(t, r.waitForRoute(context.WithoutCancel(ctx), s.routePublished()))
}

func TestBorrowBusinessRouteWaitExclusionsNeverAuthorizeReplay(t *testing.T) {
	ctx, r := WithBorrowBusinessRecovery(t.Context(), nil)
	defer r.Close()
	err := &BorrowRouteAdmissionError{Reason: "route_busy"}
	require.False(t, r.RememberAdmissionRetry(11, fmt.Errorf("database unavailable")))
	require.True(t, r.RememberAdmissionRetry(11, err))
	excluded := map[int64]struct{}{11: {}, 12: {}}
	retry, ok := r.admissionRetryExclusions(excluded)
	require.True(t, ok)
	require.NotContains(t, retry, int64(11))
	require.Contains(t, retry, int64(12), "other failed accounts stay excluded")
	require.Len(t, excluded, 2, "normal selection still tries other accounts first")
	deadline := r.routeWaitDeadline
	require.True(t, r.RememberAdmissionRetry(11, err))
	require.Equal(t, deadline, r.routeWaitDeadline)
	require.NoError(t, r.Activate(11, false))
	require.NoError(t, r.BeginTransport(ctx))
	require.False(t, r.RememberAdmissionRetry(11, err))
	_, ok = r.admissionRetryExclusions(excluded)
	require.False(t, ok, "even ambiguous transport execution prohibits the admission fallback")
}

func TestBorrowBusinessRecoveryPartialSendDisarmsWait(t *testing.T) {
	ctx, r := recoveryFixture(t)
	r.ExplicitFailure()
	require.True(t, r.ReserveRecovery(ctx, 429))
	require.NoError(t, r.BeginTransport(ctx))
	require.NoError(t, r.WritePossible())
	r.expire(ErrBorrowRecoveryTimeout)
	require.NoError(t, r.Err())
	require.Zero(t, r.sends, "partial write is not counted as completed headers")
}

func TestBorrowBusinessRecoveryRestoresClientLifecycle(t *testing.T) {
	client, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx, r := WithBorrowBusinessRecovery(client, nil)
	defer r.Close()
	require.NoError(t, r.Activate(11, false))
	require.NoError(t, r.BeginTransport(ctx))
	r.ExplicitFailure()
	require.True(t, r.ReserveRecovery(ctx, 429))
	waiting := r.Context(ctx)
	final := BorrowBusinessClientContext(waiting)
	r.expire(ErrBorrowRecoveryTimeout)
	require.NoError(t, final.Err())
	require.NoError(t, context.Cause(final))
	require.Same(t, r, BorrowBusinessRecoveryFromContext(final))
	cancel()
	require.ErrorIs(t, final.Err(), context.Canceled)
}
