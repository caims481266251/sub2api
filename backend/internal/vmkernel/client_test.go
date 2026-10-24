package vmkernel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fakeKernel(t *testing.T, h http.HandlerFunc) Binding {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "vmk-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	p := filepath.Join(dir, "k.sock")
	l, e := net.Listen("unix", p)
	if e != nil {
		t.Fatal(e)
	}
	s := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A real kernel reads its envelope before replying. With Connection: close,
		// net/http does not drain unread request bodies; replying early would race
		// the client's write and fabricate an unrelated broken-pipe failure.
		raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<20+1))
		if err != nil || len(raw) > 32<<20 {
			http.Error(w, "invalid fixture request", 400)
			return
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(raw))
		h(w, r)
	})}
	go s.Serve(l)
	t.Cleanup(func() { s.Close() })
	return Binding{Socket: p, Token: "test", Generation: 1}
}
func TestClientTrailerUsage(t *testing.T) {
	b := fakeKernel(t, func(w http.ResponseWriter, r *http.Request) {
		var env map[string]any
		json.NewDecoder(r.Body).Decode(&env)
		if env["delivery_mode"] != "realtime" {
			t.Error("wrong delivery mode")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Trailer", "X-Usage")
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"usage\":{\"input_tokens\":7,\"output_tokens\":0,\"cache_read_input_tokens\":2}}}\n\ndata: {\"type\":\"response_headers\",\"headers\":{\"anthropic-ratelimit-requests-remaining\":\"4\"}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
		w.Header().Set("X-Usage", `{"input_tokens":7,"output_tokens":9}`)
	})
	var events []string
	r, e := NewClient().Execute(context.Background(), b, Request{Protocol: "messages", Body: json.RawMessage(`{}`)}, func(e Event) error { events = append(events, e.Type); return nil })
	if e != nil {
		t.Fatal(e)
	}
	var u map[string]int
	json.Unmarshal(r.Usage, &u)
	if !r.Completed || !r.Dispatched || u["output_tokens"] != 9 || u["cache_read_input_tokens"] != 2 || r.Headers.Get("anthropic-ratelimit-requests-remaining") != "4" || events[len(events)-1] != "message_stop" {
		t.Fatalf("bad result %+v %v", r, events)
	}
}
func TestClientHTTP200Failure(t *testing.T) {
	for _, body := range []string{`{"id":"resp_bad","status":"failed","usage":{"input_tokens":0,"output_tokens":0}}`, `{"id":"resp_bad","status":"completed"}`} {
		t.Run(body, func(t *testing.T) {
			b := fakeKernel(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, body)
			})
			emitted := false
			r, e := NewClient().Execute(context.Background(), b, Request{Protocol: "responses", Body: json.RawMessage(`{}`)}, func(Event) error { emitted = true; return nil })
			if e == nil || r.Completed || emitted {
				t.Fatalf("false success %+v %v", r, e)
			}
		})
	}
}
func TestClientCancelSameAttempt(t *testing.T) {
	cancelled := make(chan string, 1)
	b := fakeKernel(t, func(w http.ResponseWriter, r *http.Request) {
		var env map[string]json.RawMessage
		json.NewDecoder(r.Body).Decode(&env)
		if r.URL.Path == "/internal/v1/cancel" {
			if r.Header.Get("x-kin-internal-token") != "test" || r.Header.Get("x-internal-token") != "test" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			var id string
			json.Unmarshal(env["request_id"], &id)
			cancelled <- id
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\"}}\n\n")
	})
	r, e := NewClient().Execute(context.Background(), b, Request{Protocol: "responses", Body: json.RawMessage(`{}`)}, func(Event) error { return fmt.Errorf("closed") })
	if e == nil || r.Completed {
		t.Fatal("false completion")
	}
	select {
	case id := <-cancelled:
		if id != r.AttemptID {
			t.Fatalf("wrong cancel id %q", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("authenticated cancellation not received")
	}
}

func TestClientDispatchBoundaryNeverRetries(t *testing.T) {
	binding := fakeKernel(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/cancel" {
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	})
	result, err := NewClient().Execute(context.Background(), binding, Request{Protocol: "responses", Body: json.RawMessage(`{}`)}, nil)
	var attempt *AttemptError
	if !errors.As(err, &attempt) || attempt.SafeToRetry || !result.Dispatched {
		t.Fatalf("unsafe post-connect retry %+v %v", result, err)
	}
	binding.Socket += "-missing"
	result, err = NewClient().Execute(context.Background(), binding, Request{Protocol: "responses", Body: json.RawMessage(`{}`)}, nil)
	if !errors.As(err, &attempt) || !attempt.SafeToRetry || result.Dispatched {
		t.Fatalf("bad no-dispatch boundary %+v %v", result, err)
	}
}
func TestClientUsageValidation(t *testing.T) {
	for _, raw := range []string{`{}`, `{"input_tokens":0}`, `{"input_tokens":-1,"output_tokens":0}`, `{"input_tokens":0,"output_tokens":0,"input_tokens_details":{"cached_tokens":-1}}`, `{"input_tokens":"0","output_tokens":0}`} {
		var usage map[string]json.RawMessage
		json.Unmarshal([]byte(raw), &usage)
		if validUsage(usage) {
			t.Fatalf("invalid usage accepted %s", raw)
		}
	}
	var zero map[string]json.RawMessage
	json.Unmarshal([]byte(`{"input_tokens":0,"output_tokens":0}`), &zero)
	if !validUsage(zero) {
		t.Fatal("explicit real zero rejected")
	}
}

func TestClientInternalAuthHeaders(t *testing.T) {
	for _, protocol := range []string{"messages", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			b := fakeKernel(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("x-kin-internal-token") != "test" || r.Header.Get("x-internal-token") != "test" {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if protocol == "responses" {
					fmt.Fprint(w, `{"id":"resp_auth","status":"completed","usage":{"input_tokens":20,"output_tokens":8}}`)
				} else {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_auth\",\"usage\":{\"input_tokens\":20,\"output_tokens\":8}}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
				}
			})
			result, err := NewClient().Execute(context.Background(), b, Request{Protocol: protocol, Body: json.RawMessage(`{}`)}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !result.Completed || len(result.Usage) == 0 {
				t.Fatal("authenticated request did not complete with usage")
			}
		})
	}
}

func TestClientSettledProviderFailurePreservesBusinessFeedback(t *testing.T) {
	b := fakeKernel(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-ID", "request-fixture")
		fmt.Fprint(w, "data: {\"type\":\"response_headers\",\"headers\":{\"x-codex-primary-used-percent\":\"95\",\"retry-after\":\"30\",\"x-codex-turn-state\":\"private-session\",\"authorization\":\"private\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_failure_fixture\",\"status\":\"failed\",\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"Rate limited\"}}}\n\n")
	})
	r, err := NewClient().Execute(context.Background(), b, Request{Protocol: "responses", Body: json.RawMessage(`{}`)}, nil)
	if err == nil || r.Completed || !r.Settled || r.HasUsage || len(r.ErrorEvent) == 0 {
		t.Fatalf("provider rejection lost or treated as success: %+v %v", r, err)
	}
	if r.RequestID != "request-fixture" || r.ResponseID != "resp_failure_fixture" || r.Headers.Get("Retry-After") != "30" || r.Headers.Get("x-codex-primary-used-percent") != "95" {
		t.Fatalf("provider feedback was not preserved: request=%q response=%q retry=%q codex-usage=%q", r.RequestID, r.ResponseID, r.Headers.Get("Retry-After"), r.Headers.Get("x-codex-primary-used-percent"))
	}
	if r.Headers.Get("Authorization") != "" || r.Headers.Get("x-codex-turn-state") != "private-session" {
		t.Fatal("kernel header allowlist violated")
	}
}

func TestClientKernelAuthenticationIsNotProviderAuthentication(t *testing.T) {
	b := fakeKernel(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"type":"authentication_error","message":"invalid internal token"}}`)
	})
	r, err := NewClient().Execute(context.Background(), b, Request{Protocol: "responses", Body: json.RawMessage(`{}`)}, nil)
	if err == nil || !r.Settled || r.Completed || len(r.ErrorEvent) > 0 {
		t.Fatal("kernel HTTP authentication failure must not revoke provider credentials")
	}
}

func TestClientWithholdsBareProviderError(t *testing.T) {
	for _, protocol := range []string{"responses", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			payload := `{"type":"error","error":{"type":"rate_limit_error","message":"Please retry later"}}`
			b := fakeKernel(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\n", payload)
			})
			emitted := 0
			result, err := NewClient().Execute(context.Background(), b, Request{Protocol: protocol, Body: json.RawMessage(`{}`)}, func(Event) error { emitted++; return nil })
			if err == nil || !result.Settled || result.Completed || string(result.ErrorEvent) != payload || emitted != 0 {
				t.Fatalf("bare failure was emitted or lost: %+v, emitted=%d, err=%v", result, emitted, err)
			}
		})
	}
}

func TestClientHTTPFailureSettlementContract(t *testing.T) {
	for _, tc := range []struct {
		name, body, terminal string
		settled              bool
	}{
		{"kernel provider wrapper", `{"type":"error","error":{"code":"provider_error","message":"model unavailable"}}`, "", true},
		{"provider rejection", `{"error":{"code":"rate_limit_exceeded","message":"retry later"}}`, "rejected", true},
		{"finished proxy transport", `{"error":{"code":"upstream_transport","message":"https://user:password@proxy.invalid"}}`, "error", true},
		{"transport without terminal", `{"error":{"type":"api_error","code":"upstream_transport","message":"error sending request"}}`, "", true},
		{"internal auth", `{"error":{"code":"internal_auth_failed"}}`, "error", true},
		{"malformed", `<html>bad gateway</html>`, "error", true},
		{"protocol", `{"error":{"code":"bad_envelope"}}`, "error", true},
		{"oversized", `{"error":{"code":"provider_error","message":"` + strings.Repeat("x", 64<<10) + `"}}`, "error", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := fakeKernel(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Terminal-State", tc.terminal)
				w.WriteHeader(502)
				fmt.Fprint(w, tc.body)
			})
			result, err := NewClient().Execute(context.Background(), b, Request{Protocol: "responses", Body: json.RawMessage(`{}`)}, nil)
			var attempt *AttemptError
			if !errors.As(err, &attempt) || attempt.SafeToRetry || !result.Dispatched || result.Settled != tc.settled || result.Completed || result.HasUsage {
				t.Fatalf("incorrect settlement: %+v %v", result, err)
			}
			wantProvider := tc.name == "kernel provider wrapper" || tc.name == "provider rejection" || tc.name == "finished proxy transport" || tc.name == "transport without terminal"
			if (len(result.ErrorEvent) > 0) != wantProvider {
				t.Fatal("provider event contract lost")
			}
			if bytes.Contains(result.ErrorEvent, []byte("password")) {
				t.Fatal("proxy credentials escaped")
			}
		})
	}
}

func TestClientCodexHTTPFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name, body, attemptCode, providerCode string
		status, providerStatus                int
	}{
		{
			name: "observed usage rejection without terminal header", status: 429,
			body:        `{"error":{"code":"usage_limit_reached","message":"upstream status 429: {\"detail\":\"Rate limit exceeded\"}","type":"api_error"}}`,
			attemptCode: "kernel_response_failed", providerCode: "usage_limit_reached", providerStatus: 429,
		},
		{
			name: "wrapped provider status takes precedence", status: 502,
			body:        `{"error":{"code":"provider_error","message":"Rate limited","status":429}}`,
			attemptCode: "kernel_response_failed", providerCode: "provider_error", providerStatus: 429,
		},
		{
			name: "provider status falls back to HTTP", status: 503,
			body:        `{"error":{"code":"provider_error","message":"Provider unavailable"}}`,
			attemptCode: "kernel_response_failed", providerCode: "provider_error", providerStatus: 503,
		},
		{
			name: "observed upstream auth without terminal header", status: 401,
			body:        `{"error":{"code":"upstream_auth","type":"api_error","message":"upstream status 401: private-provider-body"}}`,
			attemptCode: "kernel_response_failed", providerCode: "upstream_auth", providerStatus: 401,
		},
		{
			name: "upstream auth code without matching HTTP status", status: 502,
			body:        `{"error":{"code":"upstream_auth","type":"api_error","message":"Unauthorized"}}`,
			attemptCode: "kernel_status_502",
		},
		{
			name: "observed local capacity rejection", status: 503,
			body:        `{"error":{"code":"no_capacity","message":"credential busy","type":"api_error"}}`,
			attemptCode: "kernel_no_capacity",
		},
		{
			name: "local credential scheduling rejection", status: 503,
			body:        `{"error":{"code":"no_credential","message":"no schedulable credential","type":"api_error"}}`,
			attemptCode: "kernel_no_credential",
		},
		{
			name: "local sticky scheduling rejection", status: 503,
			body:        `{"error":{"code":"sticky_unavailable","type":"api_error"}}`,
			attemptCode: "kernel_sticky_unavailable",
		},
		{
			name: "internal authentication is not provider authentication", status: 401,
			body:        `{"error":{"code":"internal_auth_failed","type":"authentication_error","message":"invalid internal token"}}`,
			attemptCode: "kernel_status_401",
		},
		{
			name: "unknown HTTP 429 is not provider quota feedback", status: 429,
			body:        `{"error":{"code":"unknown_rejection","type":"api_error","message":"Rate limited"}}`,
			attemptCode: "kernel_status_429",
		},
		{
			name: "unverified usage status is not provider quota feedback", status: 503,
			body:        `{"error":{"code":"usage_limit_reached","type":"api_error"}}`,
			attemptCode: "kernel_status_503",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			binding := fakeKernel(t, func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "43")
				w.Header().Set("X-Request-ID", "request-http-failure")
				w.Header().Set("Authorization", "private-token")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			emitted := 0
			result, err := NewClient().Execute(context.Background(), binding, Request{Protocol: "responses", Body: json.RawMessage(`{}`)}, func(Event) error {
				emitted++
				return nil
			})
			var attempt *AttemptError
			if !errors.As(err, &attempt) || attempt.Code != tc.attemptCode || attempt.SafeToRetry || attempts.Load() != 1 {
				t.Fatalf("incorrect classification or replay: attempts=%d error=%v", attempts.Load(), err)
			}
			if result == nil || !result.Dispatched || !result.Settled || result.Completed || result.Accepted || result.HasUsage || emitted != 0 {
				t.Fatalf("failure became unknown execution or success: result=%+v emitted=%d", result, emitted)
			}
			if bytes.Contains(result.ErrorEvent, []byte("private-provider-body")) {
				t.Fatal("provider auth diagnostic leaked")
			}
			if result.RequestID != "request-http-failure" || result.Headers.Get("Retry-After") != "43" || result.Headers.Get("Authorization") != "" {
				t.Fatal("safe response metadata lost or private header retained")
			}
			if tc.providerStatus == 0 {
				if result.ProviderHTTPFailure || len(result.ErrorEvent) != 0 || len(result.Response) != 0 {
					t.Fatal("local or unknown rejection became provider feedback")
				}
				return
			}
			if !result.ProviderHTTPFailure {
				t.Fatal("verified HTTP failure lost its transport classification")
			}
			var event struct {
				Type  string `json:"type"`
				Error struct {
					Code    string `json:"code"`
					Status  int    `json:"status"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(result.ErrorEvent, &event) != nil || event.Type != "error" || event.Error.Code != tc.providerCode || event.Error.Status != tc.providerStatus || event.Error.Message == "" {
				t.Fatalf("provider status or error semantics lost: %s", result.ErrorEvent)
			}
		})
	}
}

func TestClientHTTPProviderFailureDropsPrivateDiagnostics(t *testing.T) {
	for _, message := range []string{"https://user:private-password@proxy.invalid", strings.Repeat("private-diagnostic ", 300)} {
		t.Run(fmt.Sprintf("message_bytes_%d", len(message)), func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"error": map[string]any{
				"code": "usage_limit_reached", "type": "api_error", "message": message,
				"debug": map[string]string{"access_token": "private-token"},
			}, "credential": "private-credential"})
			if err != nil {
				t.Fatal(err)
			}
			binding := fakeKernel(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write(body)
			})
			result, err := NewClient().Execute(context.Background(), binding, Request{Protocol: "responses", Body: json.RawMessage(`{}`)}, nil)
			if err == nil || result == nil || !result.Settled || len(result.ErrorEvent) == 0 {
				t.Fatal("sanitized provider failure was lost")
			}
			if bytes.Contains(result.ErrorEvent, []byte("private")) || !bytes.Contains(result.ErrorEvent, []byte("Upstream request failed")) {
				t.Fatalf("private or unbounded diagnostics retained: %s", result.ErrorEvent)
			}
		})
	}
}

func TestClientCancelResponseNeverInventsSettlement(t *testing.T) {
	for _, status := range []int{200, 202, 404, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			binding := fakeKernel(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/internal/v1/cancel" {
					w.WriteHeader(status)
					fmt.Fprint(w, `{"ok":true,"private":"never retain this"}`)
					return
				}
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				conn.Close()
			})
			r, err := NewClient().Execute(context.Background(), binding, Request{Protocol: "responses", Body: json.RawMessage(`{}`)}, nil)
			var attempt *AttemptError
			if !errors.As(err, &attempt) || attempt.SafeToRetry || r.Settled || r.Completed || r.CancelHTTPStatus != status {
				t.Fatalf("cancel response became settlement: %+v %v", r, err)
			}
			want := "rejected"
			if status < 300 {
				want = "acknowledged"
			}
			if r.CancelStatus != want {
				t.Fatalf("cancel status %q want %q", r.CancelStatus, want)
			}
		})
	}
}

func TestClientCodexCacheSessionEnvelopeMatchesVM2API(t *testing.T) {
	var identities, attempts []string
	b := fakeKernel(t, func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			RequestID string            `json:"request_id"`
			Body      map[string]any    `json:"body"`
			Headers   map[string]string `json:"headers"`
			Session   map[string]string `json:"session"`
		}
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			t.Fatal(err)
		}
		key, _ := env.Body["prompt_cache_key"].(string)
		if key != "stable-scoped-cache" || env.Session["session_id"] != key || env.Headers["session-id"] != key {
			t.Errorf("cache identity not propagated consistently: %+v", env)
		}
		if env.Headers["x-codex-routing-hint"] != "model=gpt-6-luna;tier=priority" {
			t.Error("missing model routing hint")
		}
		if env.Headers["authorization"] != "" {
			t.Error("caller credentials leaked")
		}
		identities = append(identities, key)
		attempts = append(attempts, env.RequestID)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"resp_ok","status":"completed","usage":{"input_tokens":100,"output_tokens":1,"input_tokens_details":{"cached_tokens":80}}}`)
	})
	for i := 0; i < 2; i++ {
		ctx := WithHeaders(WithSessionID(context.Background(), "old-connection-id"), http.Header{"Authorization": []string{"private"}})
		r, err := NewClient().Execute(ctx, b, Request{Protocol: "responses", Body: json.RawMessage(`{"model":"gpt-6-luna","service_tier":"priority","prompt_cache_key":"stable-scoped-cache"}`)}, nil)
		if err != nil || !r.Completed {
			t.Fatalf("call failed: %v", err)
		}
	}
	if identities[0] != identities[1] || attempts[0] == attempts[1] {
		t.Fatal("session must persist while attempt IDs remain distinct")
	}
}

func TestSettledHTTPQuotaMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, body      string
		reset, relative int64
	}{
		{"direct numeric", `{"error":{"code":"usage_limit_reached","type":"api_error","resets_at":1900000000,"resets_in_seconds":14716}}`, 1900000000, 14716},
		{"direct strings", `{"error":{"code":"provider_error","type":"rate_limit_exceeded","resets_at":"1900000000","resets_in_seconds":"14716"}}`, 1900000000, 14716},
		{"observed nested quota", `{"error":{"code":"usage_limit_reached","type":"api_error","message":"upstream status 429: {\"error\":{\"type\":\"usage_limit_reached\",\"resets_at\":1900000000,\"resets_in_seconds\":14716,\"message\":\"private-diagnostic\",\"credential\":\"private-secret\"}}"}}`, 1900000000, 14716},
		{"invalid resets", `{"error":{"code":"usage_limit_reached","resets_at":{},"resets_in_seconds":-1}}`, 0, 0},
		{"fractional resets", `{"error":{"code":"usage_limit_reached","resets_at":123.5,"resets_in_seconds":"9223372036854775807"}}`, 0, 0},
		{"no arbitrary message extraction", `{"error":{"code":"usage_limit_reached","message":"diagnostic upstream status 429: {\"error\":{\"type\":\"usage_limit_reached\",\"resets_at\":1900000000}}"}}`, 0, 0},
		{"unknown inner type", `{"error":{"code":"usage_limit_reached","message":"upstream status 429: {\"error\":{\"type\":\"unknown\",\"resets_at\":1900000000}}"}}`, 0, 0},
		{"transport not quota", `{"error":{"code":"upstream_transport","resets_at":1900000000}}`, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, code := settledHTTPFailure([]byte(tc.body), "", 429)
			if code != "" || len(raw) == 0 {
				t.Fatalf("missing provider event: %s", code)
			}
			var event struct {
				Error struct {
					ResetsAt        int64 `json:"resets_at"`
					ResetsInSeconds int64 `json:"resets_in_seconds"`
				} `json:"error"`
			}
			if err := json.Unmarshal(raw, &event); err != nil {
				t.Fatal(err)
			}
			if event.Error.ResetsAt != tc.reset || event.Error.ResetsInSeconds != tc.relative {
				t.Fatalf("reset metadata mismatch: %s", raw)
			}
			if bytes.Contains(raw, []byte("private-")) {
				t.Fatal("embedded private diagnostic leaked")
			}
		})
	}
}

func TestClientDisabledPromptCacheKeepsInputAndUsage(t *testing.T) {
	b := fakeKernel(t, func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			Body    map[string]any    `json:"body"`
			Headers map[string]string `json:"headers"`
			Session map[string]string `json:"session"`
		}
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			t.Fatal(err)
		}
		if env.Body["prompt_cache_key"] != nil || env.Body["prompt_cache_retention"] != nil || env.Session["session_id"] != "caller-key" || env.Headers["session-id"] != "caller-key" {
			t.Error("disabled cache must remove cache hints and preserve conversation identity")
		}
		if env.Body["input"] != "conversation" || env.Headers["x-codex-routing-hint"] != "model=gpt-6-luna" {
			t.Error("cache toggle changed request semantics")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"resp_off","status":"completed","usage":{"input_tokens":100,"output_tokens":1,"input_tokens_details":{"cached_tokens":80}}}`)
	})
	result, err := NewClient().Execute(WithSessionID(context.Background(), "old-session"), b, Request{Protocol: "responses", DisablePromptCache: true, Body: json.RawMessage(`{"model":"gpt-6-luna","input":"conversation","prompt_cache_key":"caller-key","prompt_cache_retention":"24h"}`)}, nil)
	if err != nil || result == nil || !result.Completed || !bytes.Contains(result.Usage, []byte(`"cached_tokens":80`)) {
		t.Fatalf("provider usage must remain authoritative: %v", err)
	}
}

func TestClientCodexHeadersRoundTrip(t *testing.T) {
	b := fakeKernel(t, func(w http.ResponseWriter, r *http.Request) {
		var env map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			t.Error(err)
			return
		}
		var headers map[string]string
		_ = json.Unmarshal(env["headers"], &headers)
		if headers["x-codex-turn-state"] != "fixture-state" || headers["x-codex-turn-metadata"] != "fixture-metadata" || headers["x-codex-beta-features"] != "fixture-beta" {
			t.Error("Codex context headers lost")
		}
		for _, key := range []string{"authorization", "cookie", "x-internal-token", "x-random"} {
			if headers[key] != "" {
				t.Errorf("unexpected header %s", key)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response_headers\",\"headers\":{\"x-codex-turn-state\":\"next-state\",\"authorization\":\"secret\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_next\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
	})
	h := make(http.Header)
	for _, key := range []string{"authorization", "cookie", "x-internal-token", "x-random"} {
		h.Set(key, "secret")
	}
	h.Set("x-codex-turn-state", "fixture-state")
	h.Set("x-codex-turn-metadata", "fixture-metadata")
	h.Set("x-codex-beta-features", "fixture-beta")
	ctx := WithHeaders(context.Background(), h)
	observed := ""
	ctx = WithOnResponseHeaders(ctx, func(headers http.Header) {
		if headers.Get("Authorization") != "" {
			t.Error("callback leaked credentials")
		}
		if v := headers.Get("x-codex-turn-state"); v != "" {
			observed = v
		}
	})
	r, err := NewClient().Execute(ctx, b, Request{Protocol: "responses", Body: json.RawMessage(`{"model":"gpt-6-luna","input":"hello"}`)}, func(Event) error {
		if observed != "next-state" {
			t.Error("response headers arrived after public output")
		}
		return nil
	})
	if err != nil || !r.Completed || observed != "next-state" {
		t.Fatalf("header roundtrip failed: %v", err)
	}
}
