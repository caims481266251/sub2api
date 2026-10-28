package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/vmkernel"
	"github.com/gin-gonic/gin"
)

type vmSSEFlushWriter struct {
	gin.ResponseWriter
	recorder *httptest.ResponseRecorder
	flushed  chan string
	failure  error
}

func (w *vmSSEFlushWriter) FlushError() error {
	if w.failure != nil {
		return w.failure
	}
	w.ResponseWriter.Flush()
	if w.flushed != nil {
		select {
		case w.flushed <- w.recorder.Body.String():
		default:
		}
	}
	return nil
}

func vmSSEContext(ctx context.Context) (*gin.Context, *httptest.ResponseRecorder, <-chan string) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	flushed := make(chan string, 8)
	c.Writer = &vmSSEFlushWriter{ResponseWriter: c.Writer, recorder: recorder, flushed: flushed}
	return c, recorder, flushed
}

func TestVMKernelSSEHeartbeatBeforeAndBetweenEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, recorder, flushed := vmSSEContext(ctx)
	firstToken := 73
	expected := &vmkernel.Result{Completed: true, FirstTokenMs: &firstToken}
	result, err := runVMKernelSSE(ctx, c, 15*time.Millisecond,
		func(workerCtx context.Context, emit func(vmkernel.Event) error) (*vmkernel.Result, error) {
			var first string
			select {
			case first = <-flushed:
			case <-workerCtx.Done():
				return nil, workerCtx.Err()
			}
			if strings.TrimSpace(first) != ":" {
				return nil, fmt.Errorf("first output was not a standalone SSE comment: %q", first)
			}
			if err := emit(vmkernel.Event{Type: "response.output_text.delta", Data: json.RawMessage(`{"type":"response.output_text.delta","delta":"hello"}`)}); err != nil {
				return nil, err
			}
			for {
				select {
				case snapshot := <-flushed:
					if strings.Contains(snapshot, "hello") && strings.Count(snapshot, ":\n\n") > strings.Count(first, ":\n\n") {
						return expected, nil
					}
				case <-workerCtx.Done():
					return nil, workerCtx.Err()
				}
			}
		}, func(http.Header) {}, func(event vmkernel.Event) error {
			if OpenAICompactKeepaliveAdjustedWrittenSize(c) != -1 || openAIStreamClientOutputStarted(c, false) {
				t.Error("heartbeat became semantic output before the real event")
			}
			_, err := fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event.Type, event.Data)
			if err != nil {
				return err
			}
			return FlushGatewayResponse(c.Writer)
		})
	if err != nil || result != expected || *result.FirstTokenMs != firstToken {
		t.Fatalf("heartbeat changed result or stopped midstream: result=%+v error=%v", result, err)
	}
	if strings.Count(recorder.Body.String(), ":\n\n") < 2 || strings.Count(recorder.Body.String(), "response.output_text.delta") != 2 {
		t.Fatalf("unexpected heartbeat/event framing: %q", recorder.Body.String())
	}
	if c.Writer.Header().Get("Content-Type") != "text/event-stream" || c.Writer.Header().Get("X-Accel-Buffering") != "no" {
		t.Fatalf("missing stream headers: %v", c.Writer.Header())
	}
}

func TestVMKernelSSESerializesHeaderCallbacksWithEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, recorder, flushed := vmSSEContext(ctx)
	socket := vmForwardUDS(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-flushed:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Codex-Turn-State", "fixture-header")
		for _, event := range vmForwardEvents("responses") {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
		}
	})
	client := vmkernel.NewClient()
	defer client.Close()
	var headerValue string
	var headerCalls, eventCalls int
	result, err := runVMKernelSSE(ctx, c, 15*time.Millisecond,
		func(workerCtx context.Context, emit func(vmkernel.Event) error) (*vmkernel.Result, error) {
			return client.Execute(workerCtx, vmkernel.Binding{Socket: socket, Token: "fixture", Generation: 1}, vmkernel.Request{Protocol: "responses", Body: json.RawMessage(`{}`)}, emit)
		}, func(h http.Header) {
			headerCalls++
			headerValue = h.Get("X-Codex-Turn-State")
		}, func(event vmkernel.Event) error {
			eventCalls++
			if headerValue != "fixture-header" {
				return fmt.Errorf("header callback did not precede event: %s", event.Type)
			}
			_, err := fmt.Fprintf(c.Writer, "data: %s\n\n", event.Data)
			return err
		})
	if err != nil || result == nil || !result.Completed || headerCalls == 0 || eventCalls == 0 || !strings.Contains(recorder.Body.String(), "response.completed") {
		t.Fatalf("headers/events were not serialized: headers=%d events=%d result=%+v err=%v", headerCalls, eventCalls, result, err)
	}
}

func TestVMKernelSSEFlushFailureCancelsAndJoinsWorker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, _, _ := vmSSEContext(ctx)
	c.Writer.(*vmSSEFlushWriter).failure = errors.New("fixture downstream flush failure")
	joined := make(chan struct{})
	result, err := runVMKernelSSE(ctx, c, 15*time.Millisecond,
		func(workerCtx context.Context, _ func(vmkernel.Event) error) (*vmkernel.Result, error) {
			defer close(joined)
			<-workerCtx.Done()
			return &vmkernel.Result{}, workerCtx.Err()
		}, func(http.Header) {}, func(vmkernel.Event) error { return nil })
	if err == nil || result == nil || !result.ClientDisconnect || !OpenAIDownstreamDisconnected(c) {
		t.Fatalf("downstream failure was lost: result=%+v err=%v", result, err)
	}
	select {
	case <-joined:
	default:
		t.Fatal("helper returned before the worker stopped")
	}
}

func TestVMKernelSSERequestCancellationJoinsWorker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	requestCtx, cancelRequest := context.WithCancel(ctx)
	defer cancelRequest()
	c, _, flushed := vmSSEContext(requestCtx)
	joined := make(chan struct{})
	result, err := runVMKernelSSE(ctx, c, 15*time.Millisecond,
		func(workerCtx context.Context, _ func(vmkernel.Event) error) (*vmkernel.Result, error) {
			defer close(joined)
			select {
			case <-flushed:
				cancelRequest()
			case <-workerCtx.Done():
			}
			<-workerCtx.Done()
			return &vmkernel.Result{}, workerCtx.Err()
		}, func(http.Header) {}, func(vmkernel.Event) error { return nil })
	if !errors.Is(err, context.Canceled) || result == nil || !result.ClientDisconnect {
		t.Fatalf("request cancel was lost: result=%+v err=%v", result, err)
	}
	select {
	case <-joined:
	default:
		t.Fatal("canceled request returned before its worker stopped")
	}
}

func TestVMKernelSSEEventFlushFailureJoinsAfterTrailingHeaders(t *testing.T) {
	var attempts, cancels atomic.Int32
	streamClosed := make(chan struct{})
	runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/cancel" {
			cancels.Add(1)
			return
		}
		attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Codex-Turn-State", "fixture-tail-state")
		for _, event := range vmForwardEvents("responses") {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
		}
		w.(http.Flusher).Flush()
		// Keep this handler alive so r.Context cancellation proves the host
		// closed the executing stream, not normal fixture handler completion.
		<-r.Context().Done()
		close(streamClosed)
	})
	defer runtime.kernel.Close()
	cfg := &config.Config{}
	cfg.Gateway.StreamKeepaliveInterval = 1
	gateway := &OpenAIGatewayService{cfg: cfg, vmKernel: runtime}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, _, _ := vmSSEContext(ctx)
	c.Writer.(*vmSSEFlushWriter).failure = errors.New("fixture real-event flush failure")
	result, err := vmKernelHTTP(ctx, runtime, c, account, "responses", "responses", []byte(`{"model":"gpt-5","input":"hello","stream":true}`), true, "gpt-5", gateway)
	if err == nil || result == nil || !result.ClientDisconnect || result.Completed || ctx.Err() != nil {
		t.Fatalf("event failure did not join the kernel tail cleanly: result=%+v err=%v ctx=%v", result, err, ctx.Err())
	}
	slot := runtime.slot(account.ID)
	slot.mu.Lock()
	active := slot.active
	slot.mu.Unlock()
	select {
	case <-streamClosed:
	case <-ctx.Done():
		t.Fatal("event failure did not close the executing kernel stream")
	}
	if !result.ExecutionTransportCanceled || result.CancelStatus != "stream_closed" || result.CancelHTTPStatus != 0 || result.Settled {
		t.Fatalf("event failure invented cancellation acknowledgement or settlement: %+v", result)
	}
	if active != 0 || attempts.Load() != 1 || cancels.Load() != 0 {
		t.Fatalf("event failure leaked or replayed: active=%d attempts=%d cancels=%d", active, attempts.Load(), cancels.Load())
	}
}

func TestVMKernelSSEHeartbeatDoesNotExtendReadIdle(t *testing.T) {
	var attempts, cancels atomic.Int32
	streamClosed := make(chan struct{})
	runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/cancel" {
			cancels.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(streamClosed)
	})
	defer runtime.kernel.Close()
	runtime.kernel.StreamDataIntervalTimeout = 2500 * time.Millisecond
	cfg := &config.Config{}
	cfg.Gateway.StreamKeepaliveInterval = 1
	gateway := &OpenAIGatewayService{cfg: cfg, vmKernel: runtime}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	c, recorder, _ := vmSSEContext(ctx)
	stop := WrapOpenAIResponsesHeaderWait(c, true)
	defer stop()
	result, err := vmKernelHTTP(ctx, runtime, c, account, "responses", "responses", []byte(`{"model":"gpt-5","input":"hello","stream":true}`), true, "gpt-5", gateway)
	var attempt *vmkernel.AttemptError
	if !errors.As(err, &attempt) || attempt.Code != "kernel_stream_idle_timeout" || attempt.SafeToRetry {
		t.Fatalf("heartbeats prolonged or hid read timeout: result=%+v err=%v", result, err)
	}
	if result == nil || result.Completed || result.ClientDisconnect || result.FirstTokenMs != nil || result.HasUsage || ctx.Err() != nil {
		t.Fatalf("heartbeat became output/usage or canceled the caller: result=%+v ctx=%v", result, ctx.Err())
	}
	if strings.Count(recorder.Body.String(), ":\n\n") < 2 || OpenAICompactKeepaliveAdjustedWrittenSize(c) != -1 {
		t.Fatalf("missing nonsemantic heartbeats: %q", recorder.Body.String())
	}
	slot := runtime.slot(account.ID)
	slot.mu.Lock()
	active := slot.active
	slot.mu.Unlock()
	select {
	case <-streamClosed:
	case <-ctx.Done():
		t.Fatal("read idle timeout did not close the executing kernel stream")
	}
	if !result.ExecutionTransportCanceled || result.CancelStatus != "stream_closed" || result.CancelHTTPStatus != 0 || result.Settled {
		t.Fatalf("read idle timeout invented cancellation acknowledgement or settlement: %+v", result)
	}
	if active != 0 || attempts.Load() != 1 || cancels.Load() != 0 || !VMKernelRuntimeReady(account) {
		t.Fatalf("timeout leaked concurrency, replayed, or poisoned runtime: active=%d attempts=%d cancels=%d", active, attempts.Load(), cancels.Load())
	}
	if !reportVMKernelIdleTimeout(c, "responses", err) {
		t.Fatal("idle timeout was not recognized for SSE error reporting")
	}
	var failures int
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if !json.Valid([]byte(data)) || !strings.Contains(data, "stream_timeout") {
				t.Fatalf("invalid timeout event after heartbeat: %q", line)
			}
			failures++
		}
	}
	if failures != 1 || recorder.Code != http.StatusOK || strings.Contains(recorder.Body.String(), "response.completed") {
		t.Fatalf("timeout did not terminate exactly once: code=%d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestVMKernelSSEPartialTimeoutPreservesResponseIdentity(t *testing.T) {
	const createdAt = int64(1780000000)
	runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/cancel" {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		events := vmForwardEvents("responses")
		events[0] = fmt.Sprintf(`{"type":"response.created","response":{"id":"resp_fixture","object":"response","status":"in_progress","created_at":%d,"model":"gpt-5","output":[]}}`, createdAt)
		for _, event := range events[:len(events)-1] {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	defer runtime.kernel.Close()
	runtime.kernel.StreamDataIntervalTimeout = 80 * time.Millisecond
	gateway := &OpenAIGatewayService{cfg: &config.Config{}, vmKernel: runtime}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, recorder, _ := vmSSEContext(ctx)
	result, err := vmKernelHTTP(ctx, runtime, c, account, "responses", "responses", []byte(`{"model":"gpt-5","input":"hello","stream":true}`), true, "gpt-5", gateway)
	if !reportVMKernelIdleTimeout(c, "responses", err, result) {
		t.Fatalf("partial stream did not end in read timeout: result=%+v err=%v", result, err)
	}
	var failed int
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event struct {
			Type     string `json:"type"`
			Response struct {
				ID        string `json:"id"`
				Model     string `json:"model"`
				CreatedAt int64  `json:"created_at"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil {
			t.Fatalf("invalid SSE JSON after partial timeout: %q", line)
		}
		if event.Type == "response.failed" {
			failed++
			if event.Response.ID != "resp_fixture" || event.Response.Model != "gpt-5" || event.Response.CreatedAt != createdAt {
				t.Fatalf("timeout terminal changed response identity: %+v", event.Response)
			}
		}
	}
	if failed != 1 || !strings.Contains(recorder.Body.String(), "hello") || result == nil || result.Completed || result.HasUsage {
		t.Fatalf("partial stream lost output or fabricated settlement: failed=%d result=%+v body=%q", failed, result, recorder.Body.String())
	}
}

func TestVMKernelSSEHeartbeatTimeoutErrorsByIngress(t *testing.T) {
	for _, ingress := range []string{"responses", "chat", "messages"} {
		t.Run(ingress, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			c, recorder, flushed := vmSSEContext(ctx)
			result, err := runVMKernelSSE(ctx, c, 15*time.Millisecond,
				func(workerCtx context.Context, _ func(vmkernel.Event) error) (*vmkernel.Result, error) {
					select {
					case <-flushed:
						return &vmkernel.Result{}, &vmkernel.AttemptError{Code: "kernel_stream_idle_timeout"}
					case <-workerCtx.Done():
						return nil, workerCtx.Err()
					}
				}, func(http.Header) {}, func(vmkernel.Event) error { return nil })
			if !reportVMKernelIdleTimeout(c, ingress, err, result) {
				t.Fatalf("idle timeout was not recognized: result=%+v err=%v", result, err)
			}
			body := recorder.Body.String()
			if !strings.HasPrefix(body, ":\n\n") || recorder.Code != http.StatusOK || !IsResponseCommitted(c) {
				t.Fatalf("heartbeat/error commitment changed: committed=%v status=%d body=%q", IsResponseCommitted(c), recorder.Code, body)
			}
			observed, ok := GetOpsStreamError(c)
			if !ok || !observed.CountTowardsSLA || observed.IntendedStatus != http.StatusGatewayTimeout || observed.Code != "stream_timeout" {
				t.Fatalf("committed timeout missing failure observation: found=%v error=%+v", ok, observed)
			}
			var dataFrames, eventFrames int
			var eventType string
			var payload struct {
				Type  string `json:"type"`
				Error *struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
				Response *struct {
					Status string `json:"status"`
					Error  struct {
						Code    string `json:"code"`
						Message string `json:"message"`
					} `json:"error"`
				} `json:"response"`
			}
			for _, line := range strings.Split(body, "\n") {
				if strings.HasPrefix(line, "event: ") {
					eventFrames++
					eventType = strings.TrimPrefix(line, "event: ")
				}
				if strings.HasPrefix(line, "data: ") {
					dataFrames++
					if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
						t.Fatalf("invalid timeout JSON frame: %q: %v", line, err)
					}
				}
			}
			if dataFrames != 1 {
				t.Fatalf("expected exactly one terminal error: %q", body)
			}
			if ingress == "responses" {
				if eventFrames != 1 || eventType != "response.failed" || payload.Type != "response.failed" || payload.Response == nil || payload.Response.Status != "failed" || payload.Response.Error.Code != "stream_timeout" || payload.Response.Error.Message == "" || payload.Error != nil {
					t.Fatalf("invalid Responses timeout envelope: %q", body)
				}
				return
			}
			if payload.Error == nil || payload.Error.Type != "stream_timeout" || payload.Error.Message == "" || payload.Response != nil {
				t.Fatalf("missing ingress timeout error: %q", body)
			}
			if ingress == "chat" && (eventFrames != 0 || payload.Type != "") {
				t.Fatalf("Chat timeout used a foreign event wrapper: %q", body)
			}
			if ingress == "messages" && (eventFrames != 1 || eventType != "error" || payload.Type != "error") {
				t.Fatalf("Messages timeout omitted its error event: %q", body)
			}
		})
	}
}

func TestVMKernelSSEDisabledAndFastFailures(t *testing.T) {
	for _, ingress := range []string{"responses", "chat", "messages"} {
		for _, mode := range []string{"disabled", "quick_429"} {
			t.Run(ingress+"_"+mode, func(t *testing.T) {
				runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
					if mode == "quick_429" {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusTooManyRequests)
						_, _ = fmt.Fprint(w, `{"error":{"code":"provider_error","message":"Rate limited","status":429}}`)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					for _, event := range vmForwardEvents("responses") {
						_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
					}
				})
				defer runtime.kernel.Close()
				cfg := &config.Config{}
				if mode == "quick_429" {
					cfg.Gateway.StreamKeepaliveInterval = 1
				}
				gateway := &OpenAIGatewayService{cfg: cfg, vmKernel: runtime}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				c, recorder, _ := vmSSEContext(ctx)
				stop := WrapOpenAIResponsesHeaderWait(c, true)
				defer stop()
				result, err := vmKernelHTTP(ctx, runtime, c, account, ingress, "responses", []byte(`{"model":"gpt-5","input":"hello","stream":true}`), true, "gpt-5", gateway)
				if strings.Contains(recorder.Body.String(), ":\n\n") {
					t.Fatalf("disabled/quick attempt unexpectedly wrote a heartbeat: %q", recorder.Body.String())
				}
				if mode == "quick_429" {
					if err == nil || result == nil || !result.ProviderHTTPFailure || c.Writer.Written() {
						t.Fatalf("quick 429 committed a successful stream: result=%+v err=%v", result, err)
					}
					reportVMKernelProviderError(c, account, ingress, result)
					if recorder.Code != http.StatusTooManyRequests || !json.Valid(recorder.Body.Bytes()) {
						t.Fatalf("quick 429 did not retain JSON/status: %d %s", recorder.Code, recorder.Body.String())
					}
					return
				}
				if err != nil || result == nil || !result.Completed || !strings.Contains(recorder.Body.String(), "hello") {
					t.Fatalf("disabled heartbeat changed successful stream: result=%+v err=%v", result, err)
				}
				for _, line := range strings.Split(recorder.Body.String(), "\n") {
					if strings.HasPrefix(line, "data: ") {
						payload := strings.TrimPrefix(line, "data: ")
						if payload != "[DONE]" && !json.Valid([]byte(payload)) {
							t.Fatalf("invalid %s SSE frame: %q", ingress, line)
						}
					}
				}
			})
		}
	}
}
