package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type borrowHTTPSelectionRepo struct {
	*borrowContinuationTestRepo
	checkErr error
}

func (r *borrowHTTPSelectionRepo) CheckHTTPContinuation(context.Context, int64, string, string, string) error {
	return r.checkErr
}

func TestBorrowHTTPContinuationWithProofUsesExistingScheduler(t *testing.T) {
	for _, mode := range []string{"legacy", "load_batch", "advanced"} {
		for _, failure := range []error{ErrBorrowNoEligibleRoute, ErrBorrowContinuationBindingMissing} {
			for _, normalState := range []string{"healthy", "cooldown", "model", "group", "busy"} {
				t.Run(mode+"/"+BorrowAdmissionReason(failure)+"/"+normalState, func(t *testing.T) {
					resetOpenAIAdvancedSchedulerSettingCacheForTest()
					t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
					svc, borrowed := borrowReplayFixture(t)
					base := svc.borrowRoutes.repo.(*borrowTestRepo)
					svc.borrowRoutes.repo = &borrowHTTPSelectionRepo{borrowContinuationTestRepo: &borrowContinuationTestRepo{borrowTestRepo: base}, checkErr: failure}
					borrowed.Concurrency = 5
					normal := Account{ID: 12, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 5}
					slots := schedulerTestConcurrencyCache{acquireResults: map[int64]bool{11: true, 12: true}}
					switch normalState {
					case "cooldown":
						until := time.Now().Add(time.Minute)
						normal.RateLimitResetAt = &until
					case "model":
						normal.Credentials = map[string]any{"model_mapping": map[string]any{"other": "other"}}
					case "group":
						normal.GroupIDs = []int64{99}
					case "busy":
						slots.acquireResults[12] = false
					}
					svc.accountRepo = schedulerGroupAwareOpenAIAccountRepo{schedulerTestOpenAIAccountRepo{accounts: []Account{*borrowed, normal}}}
					svc.cfg = &config.Config{}
					svc.cfg.Gateway.Scheduling.LoadBatchEnabled = mode == "load_batch"
					svc.cache = &schedulerTestGatewayCache{}
					svc.concurrencyService = NewConcurrencyService(slots)
					if mode == "advanced" {
						svc.rateLimitService = newOpenAIAdvancedSchedulerRateLimitService("true")
					}
					ctx, recovery := WithBorrowBusinessRecovery(t.Context(), nil)
					defer recovery.Close()
					ctx = svc.PrepareBorrowHTTPReplay(ctx, nil, 1, 2, "resp_proven", []byte(borrowReplayFull))
					req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(borrowReplayFull))
					ctx, body, converted, err := svc.PrepareBorrowHTTPContinuation(ctx, nil, "resp_proven", "gpt-6-astra", req.Header, []byte(borrowReplayFull))
					require.NoError(t, err)
					require.True(t, converted)
					start := time.Now()
					selected, _, err := svc.SelectAccountWithScheduler(ctx, nil, "", "", "gpt-6-astra", nil, OpenAIUpstreamTransportAny, false)
					require.Nil(t, selected)
					require.ErrorIs(t, err, failure)
					require.Less(t, time.Since(start), time.Second)
					require.True(t, recovery.routeWaitDeadline.IsZero(), "do not consume waiting before normal fallback")
					moved, _, reason := svc.MoveBorrowHTTPContinuation(req.WithContext(ctx), nil, body)
					require.Empty(t, reason)
					selected, _, err = svc.SelectAccountWithScheduler(moved.Context(), nil, "", "", "gpt-6-astra", map[int64]struct{}{11: {}}, OpenAIUpstreamTransportAny, false)
					if normalState != "healthy" && normalState != "busy" {
						require.Error(t, err)
						require.Nil(t, selected)
						return
					}
					require.NoError(t, err)
					require.EqualValues(t, 12, selected.Account.ID)
					require.Equal(t, normalState == "healthy", selected.Acquired)
					if selected.ReleaseFunc != nil {
						selected.ReleaseFunc()
					}
				})
			}
		}
	}
}

func TestBorrowHTTPContinuationRespectsTaskOwnership(t *testing.T) {
	svc, _ := borrowReplayFixture(t)
	ctx := svc.PrepareBorrowHTTPReplay(t.Context(), nil, 1, 2, "resp_proven", []byte(borrowReplayFull))
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(borrowReplayFull))
	ctx, body, _, err := svc.PrepareBorrowHTTPContinuation(ctx, nil, "resp_proven", "gpt-6-astra", req.Header, []byte(borrowReplayFull))
	require.NoError(t, err)
	ctx = context.WithValue(ctx, openAIGuardianParentAffinityContextKey{}, openAIGuardianParentAffinity{currentSessionHash: "parent"})
	moved, _, reason := svc.MoveBorrowHTTPContinuation(req.WithContext(ctx), nil, body)
	require.Nil(t, moved)
	require.Equal(t, "task_owner_bound", reason)
	require.False(t, svc.borrowHTTPContinuationCanFallback(ctx, nil))
}
