package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	specialRecovery bool
}

func (u *borrowRecoveryHandlerUpstream) Do(req *http.Request, _ string, id int64, _ int) (*http.Response, error) {
	r := service.BorrowBusinessRecoveryFromContext(req.Context())
	u.specialRecovery = u.specialRecovery || r != nil
	u.ids = append(u.ids, id)
	if u.succeedRecovery && len(u.ids) == 2 {
		body := `data: {"type":"response.output_text.delta","delta":"ok","output_index":0,"content_index":0,"item_id":"msg_test"}

data: {"type":"response.completed","response":{"id":"resp_test","model":"gpt-5.2","status":"completed","output":[{"type":"message","id":"msg_test","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}

`
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	}
	return &http.Response{StatusCode: u.status, Header: http.Header{"Content-Type": []string{"application/json"}, "Retry-After": []string{"1"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"server_error","message":"temporary upstream failure"}}`))}, nil
}

func TestBorrowBusinessUsesOrdinaryHandlerRetries(t *testing.T) {
	for _, endpoint := range []string{"responses", "chat/completions", "messages"} {
		for _, scenario := range []struct {
			name                    string
			status, accounts, calls int
			disabled                bool
		}{
			{"switch", 503, 3, 3, false}, {"429", 429, 3, 3, false}, {"single-target", 503, 1, 1, false}, {"legacy-disabled", 503, 3, 3, true}, {"recovery-success", 503, 3, 2, false},
		} {
			status := scenario.status
			t.Run(endpoint+"/"+scenario.name, func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				groupID := int64(4203)
				accounts := []service.Account{}
				for _, id := range []int64{9910, 9911, 9912}[:scenario.accounts] {
					accounts = append(accounts, service.Account{ID: id, Name: "recovery-test", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: int(id), GroupIDs: []int64{groupID}, Credentials: map[string]any{"api_key": "test-only", "base_url": "https://example.test", "pool_mode": true, "pool_mode_retry_count": float64(0), "pool_mode_retry_status_codes": []any{float64(status)}}, Extra: map[string]any{"openai_passthrough": true}})
				}
				cfg := &config.Config{RunMode: config.RunModeSimple}
				cfg.Gateway.BorrowBusinessRecoveryDisabled = scenario.disabled
				cfg.Default.RateMultiplier = 1
				cfg.Gateway.MaxAccountSwitches = 10
				repo := &openAIWSFailoverHandlerAccountRepoStub{accounts: accounts}
				upstream := &borrowRecoveryHandlerUpstream{status: status, succeedRecovery: scenario.name == "recovery-success"}
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
				require.False(t, upstream.specialRecovery, "normal endpoints must not install borrowing-specific retries")
				if status == 429 {
					require.Equal(t, 429, w.Code, w.Body.String())
				} else {
					if scenario.accounts > 1 {
						require.NotEqual(t, upstream.ids[0], upstream.ids[1])
					} else if len(upstream.ids) > 1 {
						require.Equal(t, upstream.ids[0], upstream.ids[1])
					}
					if upstream.succeedRecovery {
						require.Equal(t, 200, w.Code, w.Body.String())
						require.Contains(t, w.Body.String(), "ok")
					} else {
						require.GreaterOrEqual(t, w.Code, 500)
					}
				}
			})
		}
	}
}
