package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type borrowRecoveryHandlerUpstream struct {
	service.HTTPUpstream
	ids             []int64
	status          int
	succeedRecovery bool
	blockRecovery   bool
}

func (u *borrowRecoveryHandlerUpstream) Do(req *http.Request, _ string, id int64, _ int) (*http.Response, error) {
	r := service.BorrowBusinessRecoveryFromContext(req.Context())
	if err := r.Activate(id, false); err != nil {
		return nil, err
	}
	if err := r.BeginTransport(req.Context()); err != nil {
		return nil, err
	}
	r.WroteHeaders(req.Context())
	r.ExplicitFailure()
	u.ids = append(u.ids, id)
	if u.succeedRecovery && len(u.ids) == 2 {
		body := `data: {"type":"response.output_text.delta","delta":"ok","output_index":0,"content_index":0,"item_id":"msg_test"}

data: {"type":"response.completed","response":{"id":"resp_test","model":"gpt-5.2","status":"completed","output":[{"type":"message","id":"msg_test","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}

`
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	}
	if u.blockRecovery && len(u.ids) == 2 {
		ctx := r.Context(context.WithoutCancel(req.Context()))
		<-ctx.Done()
		return nil, context.Cause(ctx)
	}
	return &http.Response{StatusCode: u.status, Header: http.Header{"Content-Type": []string{"application/json"}, "Retry-After": []string{"1"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"server_error","message":"temporary upstream failure"}}`))}, nil
}

func TestBorrowBusinessRecoveryHandlerLoop(t *testing.T) {
	for _, endpoint := range []string{"responses", "chat/completions", "messages"} {
		for _, scenario := range []struct {
			name                    string
			status, accounts, calls int
			disabled                bool
		}{
			{"switch", 503, 3, 2, false}, {"429", 429, 3, 1, false}, {"same-target-fallback", 503, 1, 2, false}, {"rollback", 503, 3, 3, true}, {"recovery-timeout", 503, 3, 2, false}, {"recovery-success", 503, 3, 2, false},
		} {
			status := scenario.status
			t.Run(endpoint+"/"+scenario.name, func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				groupID := int64(4203)
				accounts := []service.Account{}
				for _, id := range []int64{9910, 9911, 9912}[:scenario.accounts] {
					accounts = append(accounts, service.Account{ID: id, Name: "recovery-test", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: int(id), GroupIDs: []int64{groupID}, Credentials: map[string]any{"api_key": "test-only", "base_url": "https://example.test", "pool_mode": true, "pool_mode_retry_count": float64(3), "pool_mode_retry_status_codes": []any{float64(status)}}, Extra: map[string]any{"openai_passthrough": true}})
				}
				cfg := &config.Config{RunMode: config.RunModeSimple}
				cfg.Gateway.BorrowBusinessRecoveryDisabled = scenario.disabled
				if scenario.disabled {
					for i := range accounts {
						accounts[i].Credentials["pool_mode_retry_count"] = float64(0)
					}
				}
				cfg.Default.RateMultiplier = 1
				cfg.Gateway.MaxAccountSwitches = 10
				repo := &openAIWSFailoverHandlerAccountRepoStub{accounts: accounts}
				upstream := &borrowRecoveryHandlerUpstream{status: status, blockRecovery: scenario.name == "recovery-timeout", succeedRecovery: scenario.name == "recovery-success"}
				if upstream.blockRecovery {
					cfg.Gateway.BorrowBusinessRecoveryTimeoutSeconds = 1
				}
				billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
				t.Cleanup(billing.Stop)
				gateway := service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, service.NewBillingService(cfg, nil), nil, billing, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
				h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billing, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				body := `{"model":"gpt-5.2","input":"hello","messages":[{"role":"user","content":"hello"}],"max_tokens":64,"stream":false}`
				c.Request = httptest.NewRequest("POST", "/v1/"+endpoint, strings.NewReader(body))
				c.Request.Header.Set("Content-Type", "application/json")
				c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 1803, GroupID: &groupID, User: &service.User{ID: 1703, Status: service.StatusActive}, Group: &service.Group{ID: groupID, AllowMessagesDispatch: true, Platform: service.PlatformOpenAI, Status: service.StatusActive}})
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1703, Concurrency: 0})
				switch endpoint {
				case "responses":
					h.Responses(c)
				case "chat/completions":
					h.ChatCompletions(c)
				case "messages":
					h.Messages(c)
				}
				require.Len(t, upstream.ids, scenario.calls, w.Body.String())
				if status == 429 {
					require.Equal(t, 429, w.Code, w.Body.String())
				} else {
					if scenario.accounts > 1 {
						require.NotEqual(t, upstream.ids[0], upstream.ids[1])
					} else {
						require.Equal(t, upstream.ids[0], upstream.ids[1])
					}
					if upstream.succeedRecovery {
						require.Equal(t, 200, w.Code, w.Body.String())
						require.Contains(t, w.Body.String(), "ok")
					} else {
						require.GreaterOrEqual(t, w.Code, 500)
					}
					if upstream.blockRecovery {
						require.Equal(t, 504, w.Code, w.Body.String())
						require.Contains(t, w.Body.String(), "timed out")
					}
				}
			})
		}
	}
}

func TestBorrowBusinessRecovery429ResponseCommitBoundary(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[committed], func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			ctx, r := service.WithBorrowBusinessRecovery(context.Background(), nil)
			defer r.Close()
			c.Request = c.Request.WithContext(ctx)
			require.NoError(t, r.Activate(11, false))
			require.NoError(t, r.BeginTransport(ctx))
			r.WroteHeaders(ctx)
			r.ExplicitFailure()
			if committed {
				c.Writer.Header().Set("Content-Type", "text/event-stream")
				_, _ = c.Writer.WriteString(": keepalive\n\n")
				c.Writer.Flush()
			}
			h := &OpenAIGatewayHandler{}
			failure := &service.UpstreamFailoverError{StatusCode: 429, RetryableOnSameAccount: true, SameAccountRetryDeadline: time.Now().Add(2 * time.Minute), ResponseHeaders: http.Header{"Retry-After": []string{"1"}}}
			switches := 0
			handled, retry := h.handleBorrowBusinessRecovery(c, failure, &service.Account{ID: 11}, map[int64]struct{}{}, &switches, 10, committed, false)
			require.True(t, handled)
			require.False(t, retry)
			if committed {
				require.Equal(t, 200, w.Code)
				require.Contains(t, w.Body.String(), "rate_limit")
			} else {
				require.Equal(t, 429, w.Code)
				require.Equal(t, "1", w.Header().Get("Retry-After"))
			}
		})
	}
}
