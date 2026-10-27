package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestVMKernelWSUDSReplayHooksAndUsageOnce(t *testing.T) {
	var attempts atomic.Int32
	runtime, a := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		var env struct {
			Body    json.RawMessage   `json:"body"`
			Headers map[string]string `json:"headers"`
		}
		json.NewDecoder(r.Body).Decode(&env)
		if n == 2 && gjson.GetBytes(env.Body, "input.#").Int() != 3 {
			t.Errorf("kernel did not receive expanded context: %s", env.Body)
		}
		if n == 2 && env.Headers[openAICodexTurnStateHeader] != "state-from-first-turn" {
			t.Errorf("second turn lost kernel response state")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			_, err := fmt.Fprint(w, "data: {\"type\":\"response_headers\",\"headers\":{\"x-codex-turn-state\":\"state-from-first-turn\"}}\n\n")
			if err != nil {
				t.Error(err)
			}
		}
		for _, event := range vmForwardEvents("responses") {
			fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(event, "resp_fixture", fmt.Sprintf("resp_%d", n)))
		}
	})
	s := &OpenAIGatewayService{cfg: &config.Config{}, vmKernel: runtime}
	done := make(chan error, 1)
	var order []string
	usageCount := 0
	hooks := &OpenAIWSIngressHooks{
		BeforePayloadParse: func(turn int, raw []byte, _ string) error {
			order = append(order, fmt.Sprintf("raw%d", turn))
			if turn == 2 && gjson.GetBytes(raw, "input.#").Int() != 1 {
				t.Errorf("raw frame was expanded before preflight: %s", raw)
			}
			return nil
		},
		BeforeRequest: func(turn int, body []byte, _ string) error {
			order = append(order, fmt.Sprintf("policy%d", turn))
			if turn == 2 && gjson.GetBytes(body, "input.#").Int() != 3 {
				t.Errorf("policy saw incomplete context: %s", body)
			}
			return nil
		},
		MapRequestModel: func(turn int, model string) (string, error) {
			order = append(order, fmt.Sprintf("map%d", turn))
			return model, nil
		},
		BeforeTurn: func(turn int) error { order = append(order, fmt.Sprintf("admit%d", turn)); return nil },
		AfterTurn: func(turn int, result *OpenAIForwardResult, err error) {
			order = append(order, fmt.Sprintf("after%d", turn))
			if err != nil {
				t.Errorf("turn failed %v", err)
			}
			if result == nil || !result.VMKernelUsageAuthoritative || !result.OpenAIWSMode {
				t.Errorf("bad result %+v", result)
			}
			usageCount++
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			done <- err
			return
		}
		defer conn.CloseNow()
		c, _ := gin.CreateTestContext(w)
		c.Request = r
		done <- s.proxyVMKernelWebSocket(r.Context(), c, conn, a, []byte(`{"type":"response.create","model":"gpt-5","input":"hello"}`), hooks)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()
	for turn := 1; turn <= 2; turn++ {
		for {
			_, body, err := client.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if gjson.GetBytes(body, "type").String() == "response.completed" {
				break
			}
		}
		if turn == 1 {
			err = client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5","previous_response_id":"resp_1","input":[{"role":"user","content":"next"}]}`))
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	client.CloseNow()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	expected := []string{"raw1", "policy1", "map1", "admit1", "after1", "raw2", "policy2", "map2", "admit2", "after2"}
	if !reflect.DeepEqual(order, expected) || usageCount != 2 || attempts.Load() != 2 {
		t.Fatalf("order=%v usage=%d attempts=%d", order, usageCount, attempts.Load())
	}
}

func TestVMKernelWSUDSCancelKeepsAttemptAndPartialUsage(t *testing.T) {
	cancelled := make(chan string, 1)
	var attemptID string
	runtime, a := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
		var env map[string]json.RawMessage
		json.NewDecoder(r.Body).Decode(&env)
		var id string
		json.Unmarshal(env["request_id"], &id)
		if r.URL.Path == "/internal/v1/cancel" {
			cancelled <- id
			return
		}
		attemptID = id
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_cancel\",\"model\":\"gpt-5\",\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	s := &OpenAIGatewayService{cfg: &config.Config{}, vmKernel: runtime}
	done := make(chan error, 1)
	afterCount := 0
	hooks := &OpenAIWSIngressHooks{AfterTurn: func(_ int, result *OpenAIForwardResult, err error) {
		afterCount++
		if err == nil || result == nil || !result.VMKernelUsageAuthoritative || !result.streamReadIncomplete {
			t.Errorf("bad cancellation result %+v %v", result, err)
		}
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			done <- err
			return
		}
		defer conn.CloseNow()
		c, _ := gin.CreateTestContext(w)
		c.Request = r
		done <- s.proxyVMKernelWebSocket(r.Context(), c, conn, a, []byte(`{"type":"response.create","model":"gpt-5","input":"hello"}`), hooks)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()
	if _, _, err = client.Read(ctx); err != nil {
		t.Fatal(err)
	}
	if err = client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.cancel"}`)); err != nil {
		t.Fatal(err)
	}
	_, body, err := client.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(body, "error.code").String() != "response_cancelled" || !gjson.GetBytes(body, "error.reconnect_required").Bool() {
		t.Fatalf("unexpected cancellation %s", body)
	}
	if _, _, err = client.Read(ctx); err == nil || coderws.CloseStatus(err) != coderws.StatusPolicyViolation {
		t.Fatalf("expected reconnect-required close, got %v", err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case id := <-cancelled:
		if id != attemptID {
			t.Fatalf("wrong cancel %q/%q", id, attemptID)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if afterCount != 1 {
		t.Fatalf("usage callbacks %d", afterCount)
	}
}

func TestVMKernelWSUnknownFirstHistoryNeverDispatches(t *testing.T) {
	var attempts atomic.Int32
	runtime, a := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) { attempts.Add(1); w.WriteHeader(500) })
	s := &OpenAIGatewayService{cfg: &config.Config{}, vmKernel: runtime}
	done := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			done <- err
			return
		}
		defer conn.CloseNow()
		c, _ := gin.CreateTestContext(w)
		c.Request = r
		done <- s.proxyVMKernelWebSocket(r.Context(), c, conn, a, []byte(`{"type":"response.create","model":"gpt-5","previous_response_id":"resp_foreign","input":"hello"}`), nil)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()
	select {
	case err := <-done:
		closeErr, ok := err.(*OpenAIWSClientCloseError)
		if !ok || closeErr.StatusCode() != coderws.StatusPolicyViolation {
			t.Fatalf("expected policy violation, got %v", err)
		}
	case <-ctx.Done():
		t.Fatal("history rejection timed out")
	}
	if attempts.Load() != 0 {
		t.Fatal("foreign history dispatched a guest request")
	}
}

func TestVMKernelWSReconnectReusesOwnedHistoryWithoutCrossOwnerDispatch(t *testing.T) {
	var attempts atomic.Int32
	runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		var envelope struct {
			Body json.RawMessage `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			t.Error(err)
			return
		}
		if n == 2 && gjson.GetBytes(envelope.Body, "input.#").Int() != 3 {
			t.Errorf("reconnect lost input history: %s", envelope.Body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range vmForwardEvents("responses") {
			fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(event, "resp_fixture", fmt.Sprintf("resp_vm_reconnect_%d", n)))
		}
	})
	s := &OpenAIGatewayService{cfg: &config.Config{}, vmKernel: runtime}
	done := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			done <- err
			return
		}
		defer conn.CloseNow()
		c, _ := gin.CreateTestContext(w)
		c.Request = r
		owner := int64(909121)
		if r.URL.Query().Get("foreign") != "" {
			owner++
		}
		SetOpenAIHTTPResponseOwner(c, owner, 909122)
		first := []byte(`{"type":"response.create","model":"gpt-5","input":"hello"}`)
		if r.URL.Query().Get("previous") != "" {
			first = []byte(`{"type":"response.create","model":"gpt-5","previous_response_id":"resp_vm_reconnect_1","instructions":"new turn instructions","input":[{"role":"user","content":"next"}]}`)
		}
		done <- s.proxyVMKernelWebSocket(r.Context(), c, conn, account, first, nil)
	}))
	defer server.Close()
	t.Cleanup(func() {
		vmKernelContinuations.mu.Lock()
		defer vmKernelContinuations.mu.Unlock()
		for key, entry := range vmKernelContinuations.entries {
			if key.userID == 909121 {
				vmKernelContinuations.bytes -= entry.snapshot.size
				delete(vmKernelContinuations.entries, key)
			}
		}
		vmKernelContinuations.prune(time.Now())
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, suffix := range []string{"", "?previous=1"} {
		conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+suffix, nil)
		if err != nil {
			t.Fatal(err)
		}
		for {
			_, body, readErr := conn.Read(ctx)
			if readErr != nil {
				conn.CloseNow()
				t.Fatal(readErr)
			}
			if gjson.GetBytes(body, "type").String() == "response.completed" {
				break
			}
		}
		conn.CloseNow()
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"?previous=1&foreign=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	select {
	case err := <-done:
		if _, ok := err.(*OpenAIWSClientCloseError); !ok {
			t.Fatalf("foreign owner was not rejected: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if attempts.Load() != 2 {
		t.Fatalf("unexpected guest attempts: %d", attempts.Load())
	}
}

func TestVMKernelWSProviderFailureDeliveredOnce(t *testing.T) {
	for _, mode := range []string{"response.failed", "error", "http_rejection"} {
		t.Run(mode, func(t *testing.T) {
			kind := mode
			payload := `{"type":"error","error":{"type":"invalid_request_error","code":"invalid_api_key","message":"provider rejected credentials"}}`
			if kind == "response.failed" {
				payload = `{"type":"response.failed","response":{"id":"resp_rejected","status":"failed","error":{"code":"invalid_api_key","message":"provider rejected credentials"}}}`
			}
			if mode == "http_rejection" {
				kind = "error"
				payload = `{"error":{"type":"api_error","code":"upstream_status","message":"Model unavailable","param":"model"}}`
			}
			var attempts atomic.Int32
			runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				if mode == "http_rejection" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, payload)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\n", payload)
			})
			t.Cleanup(runtime.kernel.Close)
			s := &OpenAIGatewayService{cfg: &config.Config{}, vmKernel: runtime}
			var after atomic.Int32
			done := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					done <- err
					return
				}
				defer conn.CloseNow()
				c, _ := gin.CreateTestContext(w)
				c.Request = r
				hooks := &OpenAIWSIngressHooks{AfterTurn: func(_ int, _ *OpenAIForwardResult, err error) {
					after.Add(1)
					if err == nil {
						t.Error("failed provider attempt reported success")
					}
				}}
				done <- s.proxyVMKernelWebSocket(r.Context(), c, conn, account, []byte(`{"type":"response.create","model":"gpt-5","input":"hello"}`), hooks)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			count := 0
			for {
				_, raw, err := conn.Read(ctx)
				if err != nil {
					break
				}
				if gjson.GetBytes(raw, "type").String() == kind {
					count++
					if mode == "http_rejection" {
						for key, want := range map[string]string{"type": "api_error", "code": "upstream_status", "message": "Model unavailable", "param": "model", "status": "400"} {
							if got := gjson.GetBytes(raw, "error."+key).String(); got != want {
								t.Errorf("provider %s lost: got %q want %q", key, got, want)
							}
						}
					}
				}
			}
			select {
			case err := <-done:
				if err == nil {
					t.Error("missing failure")
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if count != 1 || after.Load() != 1 || attempts.Load() != 1 {
				t.Fatalf("provider frames=%d AfterTurn=%d attempts=%d", count, after.Load(), attempts.Load())
			}
		})
	}
}

func TestVMKernelWSPolicyFailureDeliveredOnceWithoutDispatch(t *testing.T) {
	for _, kind := range []string{"cli_only", "fast", "namespace_collision"} {
		t.Run(kind, func(t *testing.T) {
			var attempts, after atomic.Int32
			runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) { attempts.Add(1) })
			s := &OpenAIGatewayService{cfg: &config.Config{}, vmKernel: runtime}
			if kind == "cli_only" {
				if account.Extra == nil {
					account.Extra = map[string]any{}
				}
				account.Extra["codex_cli_only"] = true
			} else if kind == "fast" {
				s = newOpenAIGatewayServiceWithSettings(t, &OpenAIFastPolicySettings{Rules: []OpenAIFastPolicyRule{{ServiceTier: OpenAIFastTierPriority, Action: BetaPolicyActionBlock, Scope: BetaPolicyScopeAll, ErrorMessage: "ws fast blocked", ModelWhitelist: []string{"gpt-5.5"}, FallbackAction: BetaPolicyActionPass}}})
				s.cfg = &config.Config{}
				s.vmKernel = runtime
			} else {
				account.Extra = map[string]any{"openai_responses_flatten_namespaces": true}
			}
			done := make(chan error, 1)
			httpWrites := make(chan int, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					done <- err
					return
				}
				defer conn.CloseNow()
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = r
				hooks := &OpenAIWSIngressHooks{AfterTurn: func(_ int, _ *OpenAIForwardResult, err error) {
					after.Add(1)
					if err == nil {
						t.Error("policy reported success")
					}
				}}
				frame := []byte(`{"type":"response.create","model":"gpt-5.5","service_tier":"priority","input":"hello"}`)
				if kind == "namespace_collision" {
					frame = []byte(`{"type":"response.create","model":"gpt-5.5","input":"hello","tools":[{"type":"function","name":"ns__lookup"},{"type":"namespace","name":"ns","tools":[{"type":"function","name":"lookup"}]}]}`)
				}
				err = s.proxyVMKernelWebSocket(r.Context(), c, conn, account, frame, hooks)
				httpWrites <- recorder.Body.Len()
				done <- err
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			count := 0
			for {
				_, raw, readErr := conn.Read(ctx)
				if readErr != nil {
					break
				}
				wantCode := "policy_violation"
				if kind == "namespace_collision" {
					wantCode = ""
					if !strings.Contains(gjson.GetBytes(raw, "error.message").String(), "conflicts") {
						t.Error("missing namespace collision details")
					}
				}
				if gjson.GetBytes(raw, "type").String() != "error" || gjson.GetBytes(raw, "error.code").String() != wantCode {
					t.Errorf("unexpected policy event: %s", raw)
				}
				count++
			}
			select {
			case err := <-done:
				if err == nil {
					t.Error("missing policy error")
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if count != 1 || after.Load() != 1 || attempts.Load() != 0 {
				t.Fatalf("frames=%d after=%d guest=%d", count, after.Load(), attempts.Load())
			}
			if n := <-httpWrites; n != 0 {
				t.Fatalf("policy wrote %d HTTP body bytes after WS upgrade", n)
			}
		})
	}
}
