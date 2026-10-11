package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Lifecycle/balancing are exercised against Postgres in repository integration
// tests. This fake isolates the final transport and response-binding contract.
type borrowContinuationTestRepo struct {
	*borrowTestRepo
	choice                                  *BorrowContinuationRoute
	boundResponse, boundSession, boundRoute string
}

func (r *borrowContinuationTestRepo) CheckHTTPContinuation(context.Context, int64, string, string, string) error {
	return nil
}
func (r *borrowContinuationTestRepo) CheckLatestRoute(context.Context, int64, string, string) error {
	return nil
}
func (r *borrowContinuationTestRepo) ReserveLatestRoute(ctx context.Context, id int64, identity, session, fresh, lease string) (*BorrowContinuationRoute, error) {
	return r.ReserveHTTPContinuation(ctx, id, identity, "", session, fresh, lease)
}
func (r *borrowContinuationTestRepo) ReserveHTTPContinuation(ctx context.Context, id int64, identity, response, session, fresh, lease string) (*BorrowContinuationRoute, error) {
	route, err := r.ReserveRoute(ctx, id, identity, r.choice.Route.ID, "", lease)
	if err != nil {
		return nil, err
	}
	choice := *r.choice
	choice.Route = route
	return &choice, nil
}
func (r *borrowContinuationTestRepo) BindHTTPContinuationResponse(_ context.Context, _ int64, response, route, session string) error {
	r.boundResponse, r.boundRoute, r.boundSession = response, route, session
	return nil
}

func TestBorrowHTTPContinuationScopeAndInput(t *testing.T) {
	borrow, repo := borrowFixture()
	svc := &OpenAIGatewayService{accountRepo: borrow.owner.accountRepo, borrowRoutes: borrow}
	ctx := t.Context()
	require.NoError(t, svc.getOpenAIWSStateStore().BindResponseAccount(ctx, 0, "resp_original", 11, time.Hour))
	headers := http.Header{"Session_id": []string{"client-original"}}
	body := []byte(`{"model":"gpt-6-astra","previous_response_id":"resp_original","input":[{"role":"user","content":"current input"}],"reasoning":{"effort":"high"}}`)
	effectiveCtx, effective, converted, err := svc.PrepareBorrowHTTPContinuation(ctx, nil, "resp_original", "gpt-6-astra", headers, body)
	require.NoError(t, err)
	require.True(t, converted)
	require.False(t, gjson.GetBytes(effective, "previous_response_id").Exists())
	require.Equal(t, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(effective, "input").Raw)
	require.Equal(t, "high", gjson.GetBytes(effective, "reasoning.effort").String())
	require.Equal(t, "resp_original", gjson.GetBytes(body, "previous_response_id").String())
	hint := borrowHTTPContinuationFromContext(effectiveCtx)
	require.Equal(t, borrowResponseKey("resp_original"), hint.Response)
	require.Equal(t, borrowSessionKey(headers), hint.Session)
	require.True(t, borrowHTTPContinuationAllowsAccount(effectiveCtx, 11))
	require.False(t, borrowHTTPContinuationAllowsAccount(effectiveCtx, 12))
	for _, tc := range []struct {
		name, previous, model string
		disable               bool
	}{
		{"unknown", "resp_unknown", "gpt-6-astra", false},
		{"missing model", "resp_original", "", false},
		{"disabled", "resp_original", "gpt-6-astra", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo.enabled = !tc.disable
			_, got, converted, err := svc.PrepareBorrowHTTPContinuation(ctx, nil, tc.previous, tc.model, headers, body)
			require.NoError(t, err)
			require.False(t, converted)
			require.Equal(t, body, got)
		})
	}
	repo.enabled = true
	account, _ := svc.accountRepo.GetByID(ctx, 11)
	account.Type = AccountTypeAPIKey
	_, got, converted, err := svc.PrepareBorrowHTTPContinuation(ctx, nil, "resp_original", "gpt-6-astra", headers, body)
	require.NoError(t, err)
	require.False(t, converted)
	require.Equal(t, body, got)
}

func TestBorrowHTTPContinuationRequiresPortableToolInput(t *testing.T) {
	for _, input := range []string{
		`[{"type":"function_call_output","call_id":"a","output":"x"}]`,
		`[{"type":"function_call","call_id":"a"},{"type":"function_call_output","call_id":"b"}]`,
		`[{"type":"function_call","call_id":"a"},{"type":"function_call_output"}]`,
		`[{"type":"item_reference","id":"a"}]`,
		`[{"type":"custom_tool_call_output","call_id":"a"}]`,
	} {
		require.Error(t, validateBorrowHTTPContinuationInput([]byte(`{"input":`+input+`}`)), input)
	}
	require.Error(t, validateBorrowHTTPContinuationInput([]byte(`{"input":"hello","conversation":"conv_old"}`)))
	require.NoError(t, validateBorrowHTTPContinuationInput([]byte(`{"input":"incremental text is allowed"}`)))
	require.NoError(t, validateBorrowHTTPContinuationInput([]byte(`{"input":[{"type":"function_call","call_id":"a","name":"test","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":"x"},{"type":"custom_tool_call","call_id":"b"},{"type":"custom_tool_call_output","call_id":"b","output":"y"}]}`)))
}

func TestBorrowHTTPContinuationFinalTransportAndBinding(t *testing.T) {
	for _, tc := range []struct {
		name, effective string
		restarted       bool
	}{
		{"valid original route", "", false},
		{"expired route", "new-effective-session", true},
		{"next turn after migration", "new-effective-session", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, base := borrowFixture()
			repo := &borrowContinuationTestRepo{borrowTestRepo: base, choice: &BorrowContinuationRoute{Route: base.route, SessionID: tc.effective, Restarted: tc.restarted}}
			svc.repo = repo
			ctx := context.WithValue(t.Context(), borrowHTTPContinuationKey{}, &borrowHTTPContinuation{TargetID: 11, Response: borrowResponseKey("resp_old"), Session: "original-hash", FreshSession: "new-effective-session"})
			req, err := http.NewRequestWithContext(ctx, "POST", chatgptCodexURL, strings.NewReader(`{"model":"gpt-6-astra","previous_response_id":"resp_old","input":"full current input","prompt_cache_key":"old-cache","client_metadata":{"thread_id":"old-thread","other":"preserve"}}`))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer target-secret")
			req.Header.Set("session_id", "original")
			req.Header.Set("X-Codex-Turn-State", "current-turn")
			req.Header.Set("X-Codex-Thread-ID", "old-thread")
			resp, err := svc.ForwardHTTP(req, "", 11, 10, nil, func(sent *http.Request, _ string, _ int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
				require.NoError(t, BorrowBeforeSend(sent))
				require.Equal(t, 10, n)
				body, err := io.ReadAll(sent.Body)
				require.NoError(t, err)
				replay, err := sent.GetBody()
				require.NoError(t, err)
				copy, err := io.ReadAll(replay)
				replay.Close()
				require.NoError(t, err)
				require.Equal(t, body, copy)
				require.EqualValues(t, len(body), sent.ContentLength)
				require.False(t, gjson.GetBytes(body, "previous_response_id").Exists())
				require.Equal(t, "full current input", gjson.GetBytes(body, "input").String())
				require.Equal(t, "preserve", gjson.GetBytes(body, "client_metadata.other").String())
				if tc.effective == "" {
					require.Equal(t, "original", sent.Header.Get("session_id"))
				} else {
					require.Equal(t, tc.effective, sent.Header.Get("session_id"))
					require.Equal(t, tc.effective, gjson.GetBytes(body, "prompt_cache_key").String())
					require.Equal(t, tc.effective, gjson.GetBytes(body, "client_metadata.thread_id").String())
					require.Empty(t, sent.Header.Get("X-Codex-Thread-ID"))
				}
				if tc.restarted {
					require.Empty(t, sent.Header.Get("X-Codex-Turn-State"))
				} else {
					require.Equal(t, "current-turn", sent.Header.Get("X-Codex-Turn-State"))
				}
				cookie, err := sent.Cookie("__oailb")
				require.NoError(t, err)
				require.Equal(t, "donor-cookie", cookie.Value)
				require.Equal(t, "Bearer target-secret", sent.Header.Get("Authorization"))
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_new\"}}\n\n"))}, nil
			})
			require.NoError(t, err)
			_, err = io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, borrowResponseKey("resp_new"), repo.boundResponse)
			require.Equal(t, tc.effective, repo.boundSession)
			require.Equal(t, base.route.ID, repo.boundRoute)
			require.Zero(t, activeBorrowLeases(base))
			require.Equal(t, 1, base.releases)
			require.Equal(t, "original", req.Header.Get("session_id"))
			require.Equal(t, "current-turn", req.Header.Get("X-Codex-Turn-State"))
		})
	}
}

func TestBorrowHTTPContinuationCannotEscapeTarget(t *testing.T) {
	for _, mode := range []string{"legacy", "load_batch", "advanced"} {
		t.Run(mode, func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
			borrow, repo := borrowFixture()
			// Only an unrelated healthy API-key account is available.
			account := Account{ID: 12, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 10}
			cfg := &config.Config{}
			cfg.Gateway.Scheduling.LoadBatchEnabled = mode == "load_batch"
			svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{account}}, cache: &schedulerTestGatewayCache{}, cfg: cfg, borrowRoutes: borrow, concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{})}
			if mode == "advanced" {
				svc.rateLimitService = newOpenAIAdvancedSchedulerRateLimitService("true")
			}
			ctx := context.WithValue(t.Context(), borrowHTTPContinuationKey{}, &borrowHTTPContinuation{TargetID: 11})
			selection, _, err := svc.SelectAccountWithScheduler(ctx, nil, "", "", "gpt-6-astra", nil, OpenAIUpstreamTransportAny, false)
			require.Error(t, err)
			require.Nil(t, selection)
			req := borrowRequest(t).WithContext(ctx)
			for _, id := range []int64{11, 12} {
				repo.enabled = false
				_, err = borrow.ForwardHTTP(req, "", id, 10, nil, func(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error) {
					t.Fatal("must not fall back to direct send")
					return nil, nil
				})
				require.ErrorIs(t, err, ErrBorrowUnavailable)
			}
		})
	}
}

func TestBorrowLatestRouteSessionOnlyUsesNewEffectiveIdentity(t *testing.T) {
	for _, restarting := range []bool{true, false} {
		name := "migration"
		if !restarting {
			name = "concurrent request with stale client turn-state"
		}
		t.Run(name, func(t *testing.T) {

			s, base := borrowFixture()
			req := borrowRequest(t)
			req.Header.Set("session_id", "client-session")
			req.Header.Set("X-Codex-Turn-Metadata", "old-metadata")
			// The still-valid old binding must not prevent the latest-route repository
			// from admitting and reserving the new route.
			old := *base.route
			base.oldRoutes = map[string]*BorrowRoute{old.ID: &old}
			base.sessions = map[string]string{borrowSessionKey(req.Header): old.ID}
			base.route.ID = "latest-route"
			base.route.Secret.Cookie.Value = "latest-cookie"
			repo := &borrowContinuationTestRepo{borrowTestRepo: base, choice: &BorrowContinuationRoute{Route: base.route, SessionID: "effective-latest", Restarted: restarting}}
			s.repo = repo
			ctx := WithBorrowBusinessBinding(req.Context(), req.Header, "")
			require.NoError(t, s.Check(ctx, 11, "gpt-6-astra", "http"))
			resp, err := s.ForwardHTTP(req.WithContext(ctx), "", 11, 10, nil, func(sent *http.Request, _ string, _ int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
				require.NoError(t, BorrowBeforeSend(sent))
				require.Equal(t, 10, n, "account total concurrency remains unchanged")
				require.Equal(t, "effective-latest", sent.Header.Get("session_id"))
				require.Empty(t, sent.Header.Get("X-Codex-Turn-State"))
				require.Empty(t, sent.Header.Get("X-Codex-Turn-Metadata"))
				cookie, err := sent.Cookie("__oailb")
				require.NoError(t, err)
				require.Equal(t, "latest-cookie", cookie.Value)
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_latest\"}}\n\n"))}, nil
			})
			require.NoError(t, err)
			_, err = io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, "effective-latest", repo.boundSession)
			require.Equal(t, "latest-route", repo.boundRoute)
			require.Zero(t, activeBorrowLeases(base))
			require.Equal(t, "client-session", req.Header.Get("session_id"))

		})
	}
}
