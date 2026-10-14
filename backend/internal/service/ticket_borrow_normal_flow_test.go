package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBorrowNormalFlowAdmissionNeverSendsIneligibleAccounts(t *testing.T) {
	for _, name := range []string{"disabled", "unschedulable", "expired", "rate limited", "temporary", "overload", "quota", "model runtime", "shadow", "legacy", "empty global pool"} {
		t.Run(name, func(t *testing.T) {
			s, r, accounts, _ := borrowSourceStatusFixture()
			a := accounts.account
			future, past := time.Now().Add(time.Hour), time.Now().Add(-time.Hour)
			switch name {
			case "disabled":
				a.Status = "disabled"
			case "unschedulable":
				a.Schedulable = false
			case "expired":
				a.AutoPauseOnExpired = true
				a.ExpiresAt = &past
			case "rate limited":
				a.RateLimitResetAt = &future
			case "temporary":
				a.TempUnschedulableUntil = &future
			case "overload":
				a.OverloadUntil = &future
			case "quota":
				a.Extra = map[string]any{"codex_5h_used_percent": 99.0, "auto_pause_5h_threshold": 0.95}
			case "model runtime":
				state := s.owner.openaiGatewayService.getOpenAIAccountModelTransientState()
				for range 3 {
					state.recordFailure(a.ID, openAIAccountModelTransientModel(r.source.Model), time.Now())
				}
			case "shadow":
				id := int64(99)
				a.ParentAccountID = &id
			case "legacy":
				r.source.AccountID = 0
			case "empty global pool":
				r.environmentError = ErrBorrowRotationNoProxy
			}
			up := &borrowStatusUpstream{}
			s.owner.httpUpstream = up
			_, _, err := s.acquireSource(borrowTestCandidateContext(t.Context()), r.source, BorrowSettings{})
			require.Error(t, err)
			require.Zero(t, up.calls, "invalid source must never reach transport")
		})
	}
}

func TestBorrowNormalFlowRechecksHealthAfterConnectionWait(t *testing.T) {
	for _, change := range []string{"disabled", "cooldown", "token", "global proxy disabled"} {
		t.Run(change, func(t *testing.T) {
			s, r, accounts, _ := borrowSourceStatusFixture()
			up := &boundSourceUpstream{stateProbeUpstream: &stateProbeUpstream{}}
			up.beforeSend = func() {
				a := *accounts.account
				switch change {
				case "disabled":
					a.Schedulable = false
				case "cooldown":
					until := time.Now().Add(time.Hour)
					a.RateLimitResetAt = &until
				case "token":
					a.Credentials = map[string]any{"access_token": "replacement"}
				case "global proxy disabled":
					r.environmentCheck = func() error { return ErrBorrowUnavailable }
				}
				accounts.account = &a
			}
			s.owner.httpUpstream = up
			_, _, err := s.acquireSource(borrowTestCandidateContext(t.Context()), r.source, BorrowSettings{})
			require.Error(t, err)
			require.Empty(t, up.calls, "connection acquisition must not authorize stale state")
		})
	}
}

func TestBorrowNormalFlowTerminalStreamUsesNormalClassification(t *testing.T) {
	for _, status := range []int{401, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			s, _, accounts, _ := borrowSourceStatusFixture()
			a := accounts.account
			headers := http.Header{}
			headers.Set("X-Codex-Primary-Used-Percent", "100")
			headers.Set("X-Codex-Primary-Reset-After-Seconds", "2592000")
			payload := `{"type":"error","error":{"type":"authentication_error","code":"token_revoked"}}`
			if status == 429 {
				payload = `{"type":"error","error":{"type":"usage_limit_reached","resets_in_seconds":60}}`
			}
			ctx := context.WithValue(t.Context(), openAICodexShotObserverKey{}, s.borrowShotObserver(a, "gpt-6-luna"))
			_, err := fireOpenAICodexStateShotRequest(ctx, http.Header{}, "gpt-6-luna", "", "", func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: headers, Body: io.NopCloser(strings.NewReader("data: " + payload + "\n\n"))}, nil
			})
			require.NoError(t, err)
			if status == 401 {
				require.Equal(t, 1, accounts.permanent)
				require.Equal(t, a.ID, accounts.affectedID)
			} else {
				require.WithinDuration(t, time.Now().Add(time.Minute), accounts.resetAt, 2*time.Second, "SSE 429 must not inherit HTTP 200 quota headers")
			}
		})
	}
}

func TestBorrowNormalFlowTargetErrorBelongsToTargetAndDiagnosticsStayReadOnly(t *testing.T) {
	for _, observe := range []bool{false, true} {
		s, _, accounts, _ := borrowSourceStatusFixture()
		target := *accounts.account
		target.ID = 11
		accounts.account = &target
		ctx := t.Context()
		if observe {
			ctx = context.WithValue(ctx, openAICodexShotObserverKey{}, s.borrowShotObserver(&target, "gpt-6-astra"))
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, chatgptCodexURL, nil)
		req.Header.Set("Cookie", "__oailb=original")
		up := &stateProbeUpstream{replies: []stateProbeReply{{status: 401, cookies: []string{"__oailb=reassigned"}, body: `{"error":{"code":"token_revoked"}}`}}}
		result := ProbeOpenAICodexStateRoute(ctx, up, req, "", target.ID, 1, nil)
		require.Equal(t, OpenAICodexStateInconclusive, result.Verdict)
		require.Len(t, up.calls, 1)
		if observe {
			require.Equal(t, 1, accounts.permanent)
			require.Equal(t, int64(11), accounts.affectedID)
		} else {
			require.Zero(t, accounts.statusWrites)
		}
	}
}

func TestBorrowNormalFlowFinal429DoesNotChangeBusinessRetryWindow(t *testing.T) {
	s, _, accounts, blocker := borrowSourceStatusFixture()
	gateway := s.owner.openaiGatewayService
	gateway.handleOpenAIAccountUpstreamError(t.Context(), accounts.account, 429, http.Header{}, nil, "gpt-6-luna")
	require.Equal(t, 1, blocker.retryChecks)
	require.True(t, accounts.resetAt.IsZero())
	s.borrowShotObserver(accounts.account, "gpt-6-luna")(t.Context(), openAICodexStateShot{status: 429, headers: http.Header{}})
	require.Equal(t, 1, blocker.retryChecks, "worker finalization must not start another retry window")
	require.False(t, accounts.resetAt.IsZero())
}

type borrowLeaseRaceRepo struct{ *observationWorkerRepo }

func (r *borrowLeaseRaceRepo) GrantSourceAttempt(_ context.Context, id int64, _ string) (bool, error) {
	if id == 1 {
		return false, ErrBorrowUnavailable
	}
	return true, nil
}
func TestTicketBorrowBatchRefillsAfterSourceLeaseRace(t *testing.T) {
	s, base := borrowFixture()
	base.route = nil
	r := &borrowLeaseRaceRepo{&observationWorkerRepo{borrowWorkerRepo: &borrowWorkerRepo{borrowTestRepo: base}}}
	s.repo = r
	up := &observationWorkerUpstream{}
	s.owner.httpUpstream = up
	s.runTarget(t.Context(), BorrowTarget{AcquisitionIdentity: BorrowAcquisitionSource, AccountID: 11, Revision: 2, ProbePolicy: BorrowProbePolicy{Concurrency: 1}}, BorrowSettings{Running: true}, "refill")
	require.Equal(t, 1, up.sourceCalls)
	require.NotNil(t, base.route)
	require.Equal(t, int64(2), base.route.SourceID)
}

func TestBorrowNormalFlowCachedOldTokenCannotPoisonFreshCredentials(t *testing.T) {
	s, _, accounts, _ := borrowSourceStatusFixture()
	observer := s.borrowShotObserver(accounts.account, "gpt-6-luna", "old-cached-token")
	observer(t.Context(), openAICodexStateShot{status: 401, errorBody: []byte(`{"error":{"code":"token_revoked"}}`)})
	require.Zero(t, accounts.statusWrites)
	observer = s.borrowShotObserver(accounts.account, "gpt-6-luna")
	accounts.account.Credentials["access_token"] = "new-token-in-shared-map"
	observer(t.Context(), openAICodexStateShot{status: 401, errorBody: []byte(`{"error":{"code":"token_revoked"}}`)})
	require.Zero(t, accounts.statusWrites)
}
