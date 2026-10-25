package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/vmkernel"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestVMKernelRecentRequestsHTTPAndProbe(t *testing.T) {
	for _, probe := range []bool{false, true} {
		for _, complete := range []bool{false, true} {
			t.Run(fmt.Sprintf("probe_%t_complete_%t", probe, complete), func(t *testing.T) {
				cache := &recentRequestsTestCache{}
				runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					events := vmForwardEvents("responses")
					if !complete {
						events = events[:4]
					}
					for _, event := range events {
						_, _ = fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(event, "hello", "21"))
					}
				})
				t.Cleanup(runtime.kernel.Close)
				gateway := &OpenAIGatewayService{cfg: &config.Config{}, vmKernel: runtime, recentRequests: cache}
				if probe {
					previous, loaded := vmKernelServices.Swap(PlatformOpenAI, runtime)
					t.Cleanup(func() {
						if loaded {
							vmKernelServices.Store(PlatformOpenAI, previous)
						} else {
							vmKernelServices.Delete(PlatformOpenAI)
						}
					})
					svc := &AccountTestService{accountRepo: vmForwardAccountRepo{account: account}, openaiGatewayService: gateway, recentRequests: cache}
					result, err := svc.RunPelicanBackground(context.Background(), account.ID, "gpt-6.1-sol", &PelicanTestConfig{QuestionKind: "candy", Prompt: CandyPrompt, ParallelCount: 1})
					require.NoError(t, err)
					require.Equal(t, map[bool]string{true: "success", false: "failed"}[complete], result.Status)
				} else {
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
					_, err := vmKernelHTTP(context.Background(), runtime, c, account, "responses", "responses", []byte(`{"model":"gpt-6.1-sol","input":"hello"}`), true, "gpt-6.1-sol", gateway)
					if complete {
						require.NoError(t, err)
					} else {
						require.Error(t, err)
					}
				}
				require.Len(t, cache.records, 1, "each VM transport attempt must be recorded once")
				require.Equal(t, map[bool]string{true: "success", false: "failure"}[complete], cache.records[0].Outcome)
				require.Equal(t, map[bool]string{true: "quality", false: "business"}[probe], cache.records[0].Source)
				require.NotEmpty(t, cache.records[0].AttemptID)
				if complete {
					require.NotNil(t, cache.records[0].FirstTokenMs)
				}
			})
		}
	}
}

func TestVMKernelRecentRequestsDialFailureAndLocalRejection(t *testing.T) {
	cache := &recentRequestsTestCache{}
	client := vmkernel.NewClient()
	t.Cleanup(client.Close)
	account := recentRequestsTestAccount()
	account.ExecutionMode = AccountExecutionModeVMKernel
	ctx := context.Background()
	request := vmkernel.Request{Protocol: "responses", Body: []byte(`{"model":"gpt-6.1-sol"}`)}
	local, err := client.Execute(ctx, vmkernel.Binding{}, request, nil)
	require.Error(t, err)
	require.Nil(t, local)
	recordVMKernelRecentRequest(ctx, cache, account, "gpt-6.1-sol", local, err)
	require.Empty(t, cache.records)
	result, err := client.Execute(ctx, vmkernel.Binding{Socket: filepath.Join(t.TempDir(), "missing.sock"), Token: "test-token", Generation: 1}, request, nil)
	require.Error(t, err)
	require.NotNil(t, result)
	require.NotEmpty(t, result.AttemptID)
	recordVMKernelRecentRequest(ctx, cache, account, "gpt-6.1-sol", result, err)
	require.Len(t, cache.records, 1)
	require.Equal(t, "failure", cache.records[0].Outcome)
	require.Nil(t, cache.records[0].FirstTokenMs)
}

func TestVMKernelRecentRequestsTerminalTTFT(t *testing.T) {
	for _, mode := range []string{OpenAITTFTModeSemantic, OpenAITTFTModeVisible} {
		t.Run(mode, func(t *testing.T) {
			setVMKernelTestTTFTMode(t, mode)
			for _, tc := range []struct {
				name, event         string
				complete, hasOutput bool
			}{
				{"empty_completed", `{"type":"response.completed","response":{"id":"resp_empty","status":"completed","output":[]}}`, false, false},
				{"empty_done", `{"type":"response.done","response":{"id":"resp_empty","status":"completed","output":[]}}`, false, false},
				{"failed", `{"type":"response.failed","response":{"id":"resp_failed","status":"failed","error":{"code":"server_error","message":"failed"}}}`, false, false},
				{"incomplete", `{"type":"response.incomplete","response":{"id":"resp_incomplete","status":"incomplete"}}`, false, false},
				{"bare_error", `{"type":"error","error":{"code":"invalid_request_error","message":"invalid request"}}`, false, false},
				{"completed_text", `{"type":"response.completed","response":{"id":"resp_text","status":"completed","model":"gpt-6.1-sol","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":2,"output_tokens":1}}}`, true, true},
				{"done_text", `{"type":"response.done","response":{"id":"resp_done_text","status":"completed","model":"gpt-6.1-sol","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":2,"output_tokens":1}}}`, true, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					cache := &recentRequestsTestCache{}
					runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/internal/v1/cancel" {
							w.WriteHeader(http.StatusNoContent)
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprintf(w, "data: %s\n\n", tc.event)
					})
					t.Cleanup(runtime.kernel.Close)
					gateway := &OpenAIGatewayService{cfg: &config.Config{}, vmKernel: runtime, recentRequests: cache}
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
					result, err := vmKernelHTTP(context.Background(), runtime, c, account, "responses", "responses", []byte(`{"model":"gpt-6.1-sol","input":"hello"}`), true, "gpt-6.1-sol", gateway)
					require.NotNil(t, result)
					if tc.complete {
						require.NoError(t, err)
					} else {
						require.Error(t, err)
					}
					require.Equal(t, tc.complete, result.Completed)
					require.Len(t, cache.records, 1)
					require.Equal(t, map[bool]string{true: "success", false: "failure"}[tc.complete], cache.records[0].Outcome)
					if tc.hasOutput {
						require.NotNil(t, result.FirstTokenMs)
						require.NotNil(t, cache.records[0].FirstTokenMs)
						require.Equal(t, int64(*result.FirstTokenMs), *cache.records[0].FirstTokenMs)
					} else {
						require.Nil(t, result.FirstTokenMs, "empty terminal must not become first output")
						require.Nil(t, cache.records[0].FirstTokenMs)
					}
				})
			}
		})
	}
}

func TestVMKernelRecentRequestsWebSocketEachTurn(t *testing.T) {
	var attempts atomic.Int32
	runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		id := fmt.Sprintf("resp_recent_%d", attempts.Add(1))
		for _, event := range vmForwardEvents("responses") {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(event, "resp_fixture", id))
		}
	})
	t.Cleanup(runtime.kernel.Close)
	cache := &recentRequestsTestCache{}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, vmKernel: runtime, recentRequests: cache}
	turns := make(chan error, 2)
	done := make(chan error, 1)
	hooks := &OpenAIWSIngressHooks{AfterTurn: func(_ int, _ *OpenAIForwardResult, err error) { turns <- err }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			done <- err
			return
		}
		defer conn.CloseNow()
		c, _ := gin.CreateTestContext(w)
		c.Request = r
		done <- svc.proxyVMKernelWebSocket(r.Context(), c, conn, account, []byte(`{"type":"response.create","model":"gpt-6.1-sol","input":"hello"}`), hooks)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	defer client.CloseNow()
	for turn := 0; turn < 2; turn++ {
		if turn > 0 {
			require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-6.1-sol","input":"next"}`)))
		}
		for {
			_, raw, readErr := client.Read(ctx)
			require.NoError(t, readErr)
			if gjson.GetBytes(raw, "type").String() == "response.completed" {
				break
			}
		}
		select {
		case turnErr := <-turns:
			require.NoError(t, turnErr)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	client.CloseNow()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.Len(t, cache.records, 2)
	require.Equal(t, "success", cache.records[0].Outcome)
	require.Equal(t, "success", cache.records[1].Outcome)
	require.NotEqual(t, cache.records[0].AttemptID, cache.records[1].AttemptID)
	require.NotNil(t, cache.records[0].FirstTokenMs)
	require.NotNil(t, cache.records[1].FirstTokenMs)
}
