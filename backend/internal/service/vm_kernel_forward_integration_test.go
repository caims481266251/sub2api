package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/vmkernel"
	"github.com/Wei-Shaw/sub2api/internal/vmruntime"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type vmForwardAccountRepo struct {
	AccountRepository
	account *Account
}

func (r vmForwardAccountRepo) GetByID(context.Context, int64) (*Account, error) {
	return r.account, nil
}
func vmForwardUDS(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "vm-forward-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		handler(w, r)
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	return socket
}
func vmForwardFixture(t *testing.T, platform string, handler http.HandlerFunc, setup ...func(*Account)) (*VMKernelService, *Account) {
	t.Helper()
	proxyID := int64(112233)
	a := &Account{ID: 112233, Platform: platform, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, ExecutionMode: AccountExecutionModeVMKernel, Credentials: map[string]any{"access_token": "fixture-token"}, ProxyID: &proxyID, Proxy: &Proxy{ID: proxyID, Protocol: "http", Host: "proxy.invalid", Port: 8080, Status: StatusActive}}
	if platform == PlatformAnthropic {
		a.Credentials["expires_at"] = time.Now().Add(time.Hour).Format(time.RFC3339)
		a.Credentials["scopes"] = "user:inference user:profile"
	}
	for _, apply := range setup {
		apply(a)
	}
	kernelPlatform := "codex"
	if platform == PlatformAnthropic {
		kernelPlatform = "claude"
	}
	state := vmruntime.State{Spec: vmruntime.Spec{Platform: kernelPlatform, Template: "default", CPU: 2, MemoryMiB: 2048, ProxyID: proxyID, ProxyRevision: vmProxyRevision(a)}, AccountID: a.ID, InstanceID: "fixture-instance", Generation: 1, Ready: true, GuestBootID: "boot-one", Capacity: 1, KernelSocket: vmForwardUDS(t, handler), InternalToken: "fixture-internal", AppliedCredentialRevision: vmCredentialRevision(a), AppliedProxyRevision: vmProxyRevision(a)}
	helperSocket := vmForwardUDS(t, func(w http.ResponseWriter, r *http.Request) {
		var request vmruntime.Request
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.Operation != "inspect" {
			t.Error("unexpected runtime operation")
			w.WriteHeader(400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"state": state})
	})
	helper := vmruntime.NewClient(helperSocket)
	t.Cleanup(helper.CloseIdleConnections)
	runtime := &VMKernelService{repo: vmForwardAccountRepo{account: a}, platform: platform, helper: helper, kernel: vmkernel.NewClient()}
	vmKernelReadiness.Store(a.ID, vmRuntimeStatus{state: state, credentials: vmCredentialRevision(a), proxy: vmProxyRevision(a), checked: time.Now()})
	t.Cleanup(func() { vmKernelReadiness.Delete(a.ID) })
	return runtime, a
}
func vmForwardEvents(provider string) []string {
	if provider == "messages" {
		return []string{
			`{"type":"message_start","message":{"id":"msg_fixture","type":"message","role":"assistant","model":"claude-test","content":[],"usage":{"input_tokens":2,"output_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
			`{"type":"message_stop"}`}
	}
	return []string{
		`{"type":"response.created","response":{"id":"resp_fixture","object":"response","status":"in_progress","model":"gpt-5","output":[]}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"item1","type":"message","role":"assistant","content":[]}}`,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hello"}`,
		`{"type":"response.completed","response":{"id":"resp_fixture","object":"response","status":"completed","model":"gpt-5","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":2,"output_tokens":3}}}`}
}
func TestVMKernelForwardUDSTwelveCombinations(t *testing.T) {
	for _, provider := range []string{"messages", "responses"} {
		for _, ingress := range []string{"messages", "responses", "chat"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s_%s_stream_%v", provider, ingress, stream), func(t *testing.T) {
					var attempts atomic.Int32
					platform := PlatformOpenAI
					if provider == "messages" {
						platform = PlatformAnthropic
					}
					runtime, a := vmForwardFixture(t, platform, func(w http.ResponseWriter, r *http.Request) {
						attempts.Add(1)
						var envelope struct {
							Body json.RawMessage `json:"body"`
						}
						json.NewDecoder(r.Body).Decode(&envelope)
						key := "input"
						if provider == "messages" {
							key = "messages"
							if gjson.GetBytes(envelope.Body, "betas.0").String() != "fixture-beta" {
								t.Error("beta not projected")
							}
						}
						if !gjson.GetBytes(envelope.Body, key).Exists() {
							t.Errorf("request not converted: %s", envelope.Body)
						}
						if r.Header.Get("x-kin-internal-token") != "fixture-internal" {
							t.Error("missing kernel token")
						}
						w.Header().Set("Content-Type", "text/event-stream")
						for _, event := range vmForwardEvents(provider) {
							fmt.Fprintf(w, "data: %s\n\n", event)
						}
					})
					input := map[string]any{"model": "gpt-5", "stream": stream}
					if provider == "messages" {
						input["model"] = "claude-sonnet-4-5"
					}
					if ingress == "responses" {
						input["input"] = "hello"
					} else {
						input["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
						input["max_tokens"] = 100
					}
					body, _ := json.Marshal(input)
					writer := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(writer)
					c.Request = httptest.NewRequest("POST", "/v1/test", strings.NewReader(string(body)))
					c.Request.Header.Set("anthropic-beta", "fixture-beta")
					if provider == "messages" {
						s := &GatewayService{cfg: &config.Config{}, vmKernel: runtime}
						result, err := s.forwardVMKernelMessages(context.Background(), c, a, body, ingress, nil)
						if err != nil {
							t.Fatal(err)
						}
						if result == nil || !result.VMKernelUsageAuthoritative || result.Usage.InputTokens != 2 || result.Usage.OutputTokens != 3 {
							t.Fatalf("bad result %+v", result)
						}
					} else {
						s := &OpenAIGatewayService{cfg: &config.Config{}, vmKernel: runtime}
						result, err := s.forwardVMKernelResponses(context.Background(), c, a, body, ingress, "")
						if err != nil {
							t.Fatal(err)
						}
						if result == nil || !result.VMKernelUsageAuthoritative || result.Usage.InputTokens != 2 || result.Usage.OutputTokens != 3 {
							t.Fatalf("bad result %+v", result)
						}
					}
					if attempts.Load() != 1 || !strings.Contains(writer.Body.String(), "hello") {
						t.Fatalf("attempts=%d body=%s", attempts.Load(), writer.Body.String())
					}
					if !stream && !json.Valid(writer.Body.Bytes()) {
						t.Fatalf("invalid JSON: %s", writer.Body.String())
					}
				})
			}
		}
	}
}

func TestVMKernelUnsupportedDirectSeams(t *testing.T) {
	a := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, ExecutionMode: AccountExecutionModeVMKernel}
	for _, feature := range []string{`"generate":false`, `"context_management":{}`, `"compact":true`, `"background":true`, `"tools":[{"type":"custom","name":"x"}]`} {
		body := []byte(`{"model":"gpt-5","input":"hello",` + feature + `}`)
		if err := validateVMKernelIngress(a, body); err == nil {
			t.Fatalf("accepted %s", feature)
		}
	}
	for _, path := range []string{"/v1/responses/compact", "/v1/images/generations", "/v1/messages/count_tokens"} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", path, nil)
		s := &OpenAIGatewayService{}
		if _, _, _, err := s.prepareVMKernelResponses(context.Background(), c, a, []byte(`{}`), "responses", ""); err == nil {
			t.Fatalf("accepted %s", path)
		}
	}
}

func TestVMKernelForwardPartialIsAuthoritativeAndNeverFailover(t *testing.T) {
	for _, provider := range []string{"messages", "responses"} {
		t.Run(provider, func(t *testing.T) {
			var attempts atomic.Int32
			platform := PlatformOpenAI
			if provider == "messages" {
				platform = PlatformAnthropic
			}
			runtime, a := vmForwardFixture(t, platform, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/cancel") {
					w.WriteHeader(200)
					return
				}
				attempts.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				events := vmForwardEvents(provider)
				for _, event := range events[:len(events)-1] {
					fmt.Fprintf(w, "data: %s\n\n", event)
				}
				fmt.Fprint(w, "data: {\"type\":\"error\",\"usage\":{\"input_tokens\":2,\"output_tokens\":3},\"error\":{\"message\":\"fixture failure\"}}\n\n")
			})
			body := []byte(`{"model":"gpt-5","input":"hello","stream":true}`)
			if provider == "messages" {
				body = []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}],"max_tokens":100,"stream":true}`)
			}
			writer := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(writer)
			c.Request = httptest.NewRequest("POST", "/v1/test", nil)
			var err error
			if provider == "messages" {
				s := &GatewayService{cfg: &config.Config{}, vmKernel: runtime}
				var result *ForwardResult
				result, err = s.forwardVMKernelMessages(context.Background(), c, a, body, provider, nil)
				if result == nil || !result.VMKernelUsageAuthoritative || result.Usage.OutputTokens != 3 {
					t.Fatalf("missing partial %+v err=%v cause=%v", result, err, errors.Unwrap(err))
				}
			} else {
				s := &OpenAIGatewayService{cfg: &config.Config{}, vmKernel: runtime}
				var result *OpenAIForwardResult
				result, err = s.forwardVMKernelResponses(context.Background(), c, a, body, provider, "")
				if result == nil || !result.VMKernelUsageAuthoritative || !result.streamReadIncomplete || result.Usage.OutputTokens != 3 {
					t.Fatalf("missing partial %+v err=%v cause=%v", result, err, errors.Unwrap(err))
				}
			}
			var failover *UpstreamFailoverError
			var attempt *vmkernel.AttemptError
			if err == nil || errors.As(err, &failover) || !errors.As(err, &attempt) || attempt.SafeToRetry || attempts.Load() != 1 {
				t.Fatalf("unsafe error %v attempts=%d", err, attempts.Load())
			}
			if strings.Contains(writer.Body.String(), "response.completed") || strings.Contains(writer.Body.String(), "message_stop") || strings.Contains(writer.Body.String(), "[DONE]") {
				t.Fatalf("false success: %s", writer.Body.String())
			}
		})
	}
}

func TestVMKernelClaudeAccountModelMapping(t *testing.T) {
	var attempts atomic.Int32
	runtime, a := vmForwardFixture(t, PlatformAnthropic, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		var env struct {
			Body json.RawMessage `json:"body"`
		}
		json.NewDecoder(r.Body).Decode(&env)
		if gjson.GetBytes(env.Body, "model").String() != "claude-sonnet-4-5-20250929" {
			t.Errorf("mapping lost: %s", env.Body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range vmForwardEvents("messages") {
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
	}, func(a *Account) {
		a.Credentials["model_mapping"] = map[string]any{"client-alias": "claude-sonnet-4-5-20250929"}
	})
	s := &GatewayService{cfg: &config.Config{}, vmKernel: runtime}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	result, err := s.forwardVMKernelMessages(context.Background(), c, a, []byte(`{"model":"client-alias","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`), "messages", nil)
	if err != nil || result == nil || result.Model != "client-alias" || result.UpstreamModel != "claude-sonnet-4-5-20250929" {
		t.Fatalf("mapping result %+v %v", result, err)
	}
	_, err = s.forwardVMKernelMessages(context.Background(), c, a, []byte(`{"model":"not-allowed","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`), "messages", nil)
	if err == nil || attempts.Load() != 1 {
		t.Fatalf("allowlist bypass %v attempts=%d", err, attempts.Load())
	}
}
