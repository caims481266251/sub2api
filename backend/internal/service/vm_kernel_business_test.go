package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/vmkernel"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestVMKernelBusinessErrorRetriesOnlyUnsubmittedTransport(t *testing.T) {
	for _, tc := range []struct {
		name, code              string
		dispatched, safe, retry bool
	}{
		{"before dial", "kernel_transport_failed", false, true, true},
		{"busy", "vm_runtime_busy", false, true, true},
		{"unknown", "kernel_transport_failed", true, false, false},
		{"contradictory", "kernel_transport_failed", true, true, false},
		{"validation", "kernel_body_invalid", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := vmKernelBusinessError(context.Background(), &vmkernel.Result{Dispatched: tc.dispatched}, &vmkernel.AttemptError{Code: tc.code, SafeToRetry: tc.safe})
			var failover *UpstreamFailoverError
			if errors.As(err, &failover) != tc.retry {
				t.Fatalf("incorrect retry boundary: %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := vmKernelBusinessError(ctx, nil, &vmkernel.AttemptError{Code: "kernel_transport_failed", SafeToRetry: true})
	var failover *UpstreamFailoverError
	if errors.As(err, &failover) {
		t.Fatal("cancelled request must not switch accounts")
	}
}

func TestVMKernelProviderErrorUsesOriginalClassification(t *testing.T) {
	for _, tc := range []struct {
		payload string
		status  int
	}{
		{`{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded"}}}`, 429},
		{`{"type":"response.failed","response":{"error":{"type":"authentication_error","code":"invalid_api_key"}}}`, 401},
		{`{"type":"response.failed","response":{"error":{"type":"invalid_request_error"}}}`, 400},
		{`{"type":"error","error":{"type":"server_error"}}`, 500},
	} {
		if status := vmKernelProviderErrorStatus([]byte(tc.payload)); status != tc.status {
			t.Fatalf("status %d want %d", status, tc.status)
		}
	}
}

func TestVMKernelProviderFailurePreservesStatusAndRetryAfter(t *testing.T) {
	for _, ingress := range []string{"responses", "chat", "messages"} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/v1/test", nil)
		r := &vmkernel.Result{Headers: http.Header{"Retry-After": []string{"42"}}, ErrorEvent: []byte(`{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","message":"Please retry later"}}}`)}
		reportVMKernelProviderError(c, &Account{Platform: PlatformOpenAI}, ingress, r)
		if w.Code != 429 || w.Header().Get("Retry-After") != "42" {
			t.Fatalf("status lost for %s: %d", ingress, w.Code)
		}
	}
}

func TestVMKernelHTTPRateLimitWithoutTerminalPreservesReadyRuntime(t *testing.T) {
	var attempts atomic.Int32
	runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "43")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"code":"usage_limit_reached","message":"upstream status 429: {\"detail\":\"Rate limit exceeded\"}"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range vmForwardEvents("responses") {
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
	}, func(account *Account) { account.Concurrency = 30 })
	// The ordinary rate-limit service owns cooldown. A nil service isolates the
	// VM layer so a settled provider rejection must not latch a second cooldown.
	svc := &OpenAIGatewayService{vmKernel: runtime}
	body := []byte(`{"model":"gpt-5.5","input":"hello","stream":true}`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil).WithContext(ctx)
	result, err := svc.forwardVMKernelResponses(ctx, c, account, body, "responses", "")
	var attempt *vmkernel.AttemptError
	if result != nil || !errors.As(err, &attempt) || attempt.Code != "kernel_response_failed" {
		t.Fatalf("provider rate limit was not settled: result=%+v err=%v", result, err)
	}
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "43" {
		t.Fatalf("provider rate limit metadata lost: status=%d retry-after=%q body=%s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	if !strings.Contains(gjson.Get(w.Body.String(), "error.message").String(), "Rate limit exceeded") {
		t.Fatalf("provider rate limit message lost: %s", w.Body.String())
	}
	if !VMKernelRuntimeReady(account) || runtime.slot(account.ID).active != 0 {
		t.Fatal("settled provider rate limit changed VM readiness or retained an execution slot")
	}

	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil).WithContext(ctx)
	result, err = svc.forwardVMKernelResponses(ctx, c, account, body, "responses", "")
	if err != nil || result == nil || !result.VMKernelUsageAuthoritative || result.UpstreamTerminalEvent != "response.completed" {
		t.Fatalf("same VM did not accept a successful follow-up: result=%+v err=%v", result, err)
	}
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "hello") || attempts.Load() != 2 {
		t.Fatalf("follow-up was not forwarded exactly once: status=%d attempts=%d body=%s", w.Code, attempts.Load(), w.Body.String())
	}
	if !VMKernelRuntimeReady(account) || runtime.slot(account.ID).active != 0 {
		t.Fatal("successful follow-up changed VM readiness or retained an execution slot")
	}
}

func TestVMKernelForwardHasNoFixedTwoHundredRequestLimit(t *testing.T) {
	const requests = 205
	var attempts atomic.Int32
	runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
		attempt := attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range vmForwardEvents("responses") {
			event = strings.ReplaceAll(event, "resp_fixture", fmt.Sprintf("resp_vm_repeat_%d", attempt))
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
	}, func(account *Account) { account.Concurrency = 30 })
	svc := &OpenAIGatewayService{vmKernel: runtime}
	body := []byte(`{"model":"gpt-5.5","input":"hello","stream":false}`)
	// All responses come from the local UDS fixture. Keeping the whole sequence
	// below one minute catches an accidental VM-specific 200-RPM admission cap.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for i := 1; i <= requests; i++ {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil).WithContext(ctx)
		result, err := svc.forwardVMKernelResponses(ctx, c, account, body, "responses", "")
		if err != nil || result == nil || !result.VMKernelUsageAuthoritative || result.UpstreamTerminalEvent != "response.completed" {
			t.Fatalf("request %d failed: result=%+v err=%v", i, result, err)
		}
		if w.Code != http.StatusOK || gjson.Get(w.Body.String(), "status").String() != "completed" {
			t.Fatalf("request %d did not complete publicly: status=%d body=%s", i, w.Code, w.Body.String())
		}
	}
	if attempts.Load() != requests || !VMKernelRuntimeReady(account) || runtime.slot(account.ID).active != 0 {
		t.Fatalf("fixed local cap or execution slot leak: attempts=%d ready=%v active=%d", attempts.Load(), VMKernelRuntimeReady(account), runtime.slot(account.ID).active)
	}
}

func TestVMKernelPreparationPoliciesDoNotWriteHTTP(t *testing.T) {
	for _, policy := range []string{"cli", "fast"} {
		t.Run(policy, func(t *testing.T) {
			svc := newOpenAIGatewayServiceWithSettings(t, &OpenAIFastPolicySettings{Rules: []OpenAIFastPolicyRule{{ServiceTier: OpenAIFastTierPriority, Action: BetaPolicyActionBlock, Scope: BetaPolicyScopeAll, ErrorMessage: "fast blocked", ModelWhitelist: []string{"gpt-5.5"}, FallbackAction: BetaPolicyActionPass}}})
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ExecutionMode: AccountExecutionModeVMKernel}
			if policy == "cli" {
				account.Extra = map[string]any{"codex_cli_only": true}
			}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			_, _, _, err := svc.prepareVMKernelResponses(context.Background(), c, account, []byte(`{"model":"gpt-5.5","input":"hello","service_tier":"priority"}`), "responses", "")
			if err == nil {
				t.Fatal("policy was bypassed")
			}
			if c.Writer.Written() || w.Body.Len() != 0 {
				t.Fatal("shared preparation wrote HTTP output")
			}
			reason, _ := c.Get(OpsClientBusinessLimitedReasonKey)
			if reason != OpsClientBusinessLimitedReasonLocalPolicyDenied {
				t.Fatal("original policy classification lost")
			}
			var typed *vmKernelRequestPolicyError
			var fast *OpenAIFastBlockedError
			if policy == "cli" && !errors.As(err, &typed) {
				t.Fatalf("missing CLI policy metadata: %v", err)
			}
			if policy == "fast" && !errors.As(err, &fast) {
				t.Fatalf("missing fast policy metadata: %v", err)
			}
			reportVMKernelPreparationError(c, "responses", err)
			wantType := "forbidden_error"
			if policy == "fast" {
				wantType = "permission_error"
			}
			if w.Code != http.StatusForbidden || gjson.Get(w.Body.String(), "error.type").String() != wantType {
				t.Fatalf("policy response changed: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestVMKernelBareProviderErrorPresentedOnce(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial_%v", partial), func(t *testing.T) {
			runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if partial {
					fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
				}
				fmt.Fprint(w, "data: {\"type\":\"error\",\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"Please retry later\"}}\n\n")
			})
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			svc := &OpenAIGatewayService{vmKernel: runtime}
			_, err := svc.forwardVMKernelResponses(context.Background(), c, account, []byte(`{"model":"gpt-5.5","input":"hello","stream":true}`), "responses", "")
			if err == nil {
				t.Fatal("provider error treated as success")
			}
			wantStatus := http.StatusTooManyRequests
			if partial {
				wantStatus = http.StatusOK
			}
			if w.Code != wantStatus || strings.Count(w.Body.String(), "Please retry later") != 1 {
				t.Fatalf("wrong status or duplicate failure: %d %s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "rate_limit_exceeded") {
				t.Fatal("raw provider error escaped normalized output")
			}
		})
	}
}

func TestVMKernelProviderWrapperPreservesExplicitStatus(t *testing.T) {
	for _, status := range []int{401, 403, 429, 529} {
		payload := []byte(fmt.Sprintf(`{"type":"error","error":{"type":"provider_error","status":%d,"message":"Provider rejected request"}}`, status))
		if got := vmKernelProviderErrorStatus(payload); got != status {
			t.Fatalf("provider wrapper lost status: got %d want %d", got, status)
		}
	}
}
