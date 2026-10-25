package handler

import (
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
	specialRecovery bool
	borrow          bool
	retryAfter      string
}

func (u *borrowRecoveryHandlerUpstream) Do(req *http.Request, _ string, id int64, _ int) (*http.Response, error) {
	r := service.BorrowBusinessRecoveryFromContext(req.Context())
	if u.borrow {
		if err := r.Activate(id, false); err != nil {
			return nil, err
		}
		if err := r.BeginTransport(req.Context()); err != nil {
			return nil, err
		}
		r.WroteHeaders(req.Context())
		r.ExplicitFailure()
	}
	u.specialRecovery = u.specialRecovery || r.Active()
	u.ids = append(u.ids, id)
	if u.succeedRecovery && len(u.ids) == 2 {
		body := `data: {"type":"response.output_text.delta","delta":"ok","output_index":0,"content_index":0,"item_id":"msg_test"}

data: {"type":"response.completed","response":{"id":"resp_test","model":"gpt-5.2","status":"completed","output":[{"type":"message","id":"msg_test","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}

`
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	}
	retryAfter := u.retryAfter
	if retryAfter == "" {
		retryAfter = "1"
	}
	return &http.Response{StatusCode: u.status, Header: http.Header{"Content-Type": []string{"application/json"}, "Retry-After": []string{retryAfter}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"server_error","message":"temporary upstream failure"}}`))}, nil
}

func TestBorrowBusinessUsesOrdinaryHandlerRetries(t *testing.T) {
	for _, endpoint := range []string{"responses", "chat/completions", "messages"} {
		for _, scenario := range []struct {
			name                    string
			status, accounts, calls int
			disabled                bool
			borrow                  bool
		}{
			{"switch", 503, 3, 3, false, false}, {"429", 429, 3, 3, false, false}, {"single-target", 503, 1, 1, false, false}, {"legacy-disabled", 503, 3, 3, true, false}, {"recovery-success", 503, 3, 2, false, false},
			{"borrow-503", 503, 3, 2, false, true}, {"borrow-429", 429, 3, 2, false, true}, {"borrow-single-429", 429, 1, 1, false, true},
			{"borrow-429-independent-success", 429, 3, 2, false, true},
		} {
			status := scenario.status
			t.Run(endpoint+"/"+scenario.name, func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				groupID := int64(4203)
				accounts := []service.Account{}
				for _, id := range []int64{9910, 9911, 9912}[:scenario.accounts] {
					accounts = append(accounts, service.Account{ID: id, Name: "recovery-test", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: int(id), GroupIDs: []int64{groupID}, Credentials: map[string]any{"api_key": "test-only", "base_url": "https://example.test", "pool_mode": true, "pool_mode_retry_count": float64(0), "pool_mode_retry_status_codes": []any{float64(status)}}, Extra: map[string]any{"openai_passthrough": true}})
				}
				if scenario.name == "borrow-429-independent-success" {
					accounts[0].Credentials["base_url"] = "https://origin.example/v1"
					for i := 1; i < len(accounts); i++ {
						accounts[i].Credentials["base_url"] = "https://fallback.test/v1"
					}
				}
				cfg := &config.Config{RunMode: config.RunModeSimple}
				cfg.Gateway.BorrowBusinessRecoveryDisabled = scenario.disabled
				cfg.Default.RateMultiplier = 1
				cfg.Gateway.MaxAccountSwitches = 10
				repo := &openAIWSFailoverHandlerAccountRepoStub{accounts: accounts}
				upstream := &borrowRecoveryHandlerUpstream{status: status, borrow: scenario.borrow, succeedRecovery: strings.HasSuffix(scenario.name, "success")}
				if scenario.name == "borrow-429-independent-success" {
					upstream.retryAfter = "32"
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
				started := time.Now()
				switch endpoint {
				case "responses":
					h.Responses(c)
				case "chat/completions":
					h.ChatCompletions(c)
				case "messages":
					h.Messages(c)
				}
				if scenario.name == "borrow-429-independent-success" {
					require.Less(t, time.Since(started), 5*time.Second, "independent fallback must not consume the recovery budget waiting for the failed target")
				}
				require.NoError(t, c.Request.Context().Err(), "finishing recovery must not look like a client disconnect")
				if scenario.name == "borrow-single-429" {
					require.GreaterOrEqual(t, len(upstream.ids), 1)
					require.LessOrEqual(t, len(upstream.ids), 2, "single-account fallback must share the same budget")
				} else {
					require.Len(t, upstream.ids, scenario.calls, w.Body.String())
				}
				require.Equal(t, scenario.borrow, upstream.specialRecovery, "ordinary requests must not activate borrowing-specific retries")
				if status == 429 && !upstream.succeedRecovery {
					expected := 429
					if scenario.borrow {
						expected = 503
					}
					require.Equal(t, expected, w.Code, w.Body.String())
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

func TestBorrowRecoveryDelayHonorsUpstream(t *testing.T) {
	for _, raw := range []string{"2", "90000", time.Now().Add(20 * time.Second).UTC().Format(http.TimeFormat)} {
		f := &service.UpstreamFailoverError{ResponseHeaders: http.Header{"Retry-After": []string{raw}}}
		require.GreaterOrEqual(t, borrowRecoveryDelay(f, time.Now()), 2*time.Second)
	}
	f := &service.UpstreamFailoverError{SameAccountRetryDelay: 8 * time.Second}
	require.GreaterOrEqual(t, borrowRecoveryDelay(f, time.Now()), 8*time.Second)
}
