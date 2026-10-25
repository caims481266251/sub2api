package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func setVMKernelTestTTFTMode(t *testing.T, mode string) {
	t.Helper()
	previous := gatewayForwardingCache.Load()
	gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{openAITTFTMode: mode, expiresAt: time.Now().Add(time.Minute).UnixNano()})
	t.Cleanup(func() {
		if previous != nil {
			gatewayForwardingCache.Store(previous)
		} else {
			gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{openAITTFTMode: OpenAITTFTModeSemantic})
		}
	})
}

// Separate the preamble, semantic progress and nonempty output so the reported
// time identifies which event was counted, even when all events are forwarded.
func writeVMKernelTTFTStream(w io.Writer, responseID string) {
	write := func(event string) {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(event, "resp_fixture", responseID))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
	events := vmForwardEvents("responses")
	write(events[0])
	write(`{"type":"response.in_progress"}`)
	write(`{"type":"keepalive"}`)
	time.Sleep(80 * time.Millisecond)
	write(events[1])
	write(events[2])
	write(`{"type":"response.output_text.delta","delta":""}`)
	time.Sleep(160 * time.Millisecond)
	for _, event := range events[3:] {
		write(event)
	}
}

func requireVMKernelTTFTPhase(t *testing.T, firstTokenMs *int, mode string) {
	t.Helper()
	require.NotNil(t, firstTokenMs)
	if mode == OpenAITTFTModeVisible {
		require.GreaterOrEqual(t, *firstTokenMs, 230, "preamble, structural progress and empty delta must not count as visible output")
	} else {
		require.GreaterOrEqual(t, *firstTokenMs, 70, "lifecycle metadata and keepalive must not count as semantic output")
		require.Less(t, *firstTokenMs, 200, "semantic TTFT must not wait for the text delta")
	}
}

func TestVMKernelResponsesTTFTMatchesOrdinaryHTTP(t *testing.T) {
	for _, mode := range []string{"", OpenAITTFTModeSemantic, OpenAITTFTModeVisible} {
		name := mode
		if name == "" {
			name = "default"
		}
		t.Run(name, func(t *testing.T) {
			setVMKernelTestTTFTMode(t, mode)
			for _, path := range []string{"ordinary", "ordinary_passthrough", "docker_sse", "docker_json", "docker_chat", "docker_messages"} {
				t.Run(path, func(t *testing.T) {
					svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
					var firstTokenMs *int
					if strings.HasPrefix(path, "ordinary") {
						reader, writer := io.Pipe()
						t.Cleanup(func() { _ = reader.Close() })
						done := make(chan struct{})
						go func() {
							defer close(done)
							defer writer.Close()
							writeVMKernelTTFTStream(writer, "resp_fixture")
						}()
						resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: reader}
						account := &Account{ID: 1, Platform: PlatformOpenAI}
						if path == "ordinary_passthrough" {
							result, err := svc.handleStreamingResponsePassthrough(context.Background(), resp, c, account, time.Now(), "gpt-5", "gpt-5")
							require.NoError(t, err)
							firstTokenMs = result.firstTokenMs
						} else {
							result, err := svc.handleStreamingResponse(context.Background(), resp, c, account, time.Now(), "gpt-5", "gpt-5")
							require.NoError(t, err)
							firstTokenMs = result.firstTokenMs
						}
						<-done
					} else {
						runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, _ *http.Request) {
							w.Header().Set("Content-Type", "text/event-stream")
							writeVMKernelTTFTStream(w, "resp_fixture")
						})
						t.Cleanup(runtime.kernel.Close)
						svc.vmKernel = runtime
						ingress := "responses"
						if path == "docker_chat" {
							ingress = "chat"
						} else if path == "docker_messages" {
							ingress = "messages"
						}
						result, err := vmKernelHTTP(context.Background(), runtime, c, account, ingress, "responses", []byte(`{"model":"gpt-5","input":"hello"}`), path != "docker_json", "gpt-5", svc)
						require.NoError(t, err)
						require.True(t, result.Completed)
						firstTokenMs = result.FirstTokenMs
					}
					requireVMKernelTTFTPhase(t, firstTokenMs, mode)
				})
			}
		})
	}
}

func TestVMKernelWSTTFTFollowsModePerTurn(t *testing.T) {
	setVMKernelTestTTFTMode(t, "")
	var attempts atomic.Int32
	runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeVMKernelTTFTStream(w, fmt.Sprintf("resp_ttft_%d", attempts.Add(1)))
	})
	t.Cleanup(runtime.kernel.Close)
	svc := &OpenAIGatewayService{cfg: &config.Config{}, vmKernel: runtime}
	type turnResult struct {
		result *OpenAIForwardResult
		err    error
	}
	results := make(chan turnResult, 3)
	done := make(chan error, 1)
	hooks := &OpenAIWSIngressHooks{AfterTurn: func(turn int, result *OpenAIForwardResult, err error) {
		mode := OpenAITTFTModeVisible
		if turn == 2 {
			mode = OpenAITTFTModeSemantic
		}
		gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{openAITTFTMode: mode, expiresAt: time.Now().Add(time.Minute).UnixNano()})
		results <- turnResult{result, err}
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
		done <- svc.proxyVMKernelWebSocket(r.Context(), c, conn, account, []byte(`{"type":"response.create","model":"gpt-5","input":"hello"}`), hooks)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	defer client.CloseNow()
	for turn, mode := range []string{OpenAITTFTModeSemantic, OpenAITTFTModeVisible, OpenAITTFTModeSemantic} {
		if turn > 0 {
			require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5","input":"next"}`)))
		}
		for {
			_, raw, err := client.Read(ctx)
			require.NoError(t, err)
			if gjson.GetBytes(raw, "type").String() == "response.completed" {
				break
			}
		}
		select {
		case outcome := <-results:
			require.NoError(t, outcome.err)
			require.NotNil(t, outcome.result)
			requireVMKernelTTFTPhase(t, outcome.result.FirstTokenMs, mode)
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
}
