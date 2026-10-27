package vmkernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const idleCompletedEvent = `data: {"type":"response.completed","response":{"id":"resp_idle","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"

func idleTestClient(t *testing.T, timeout time.Duration) *Client {
	t.Helper()
	client := NewClient()
	client.StreamDataIntervalTimeout = timeout
	t.Cleanup(client.Close)
	return client
}

func TestClientStreamIdleTimeout(t *testing.T) {
	for _, phase := range []string{"before_event", "after_completed", "error_body"} {
		t.Run(phase, func(t *testing.T) {
			var requests, cancels atomic.Int32
			var cancelID atomic.Value
			binding := fakeKernel(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/internal/v1/cancel" {
					cancels.Add(1)
					var body struct {
						RequestID string `json:"request_id"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					cancelID.Store(body.RequestID)
					w.WriteHeader(http.StatusNoContent)
					return
				}
				requests.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				if phase == "error_body" {
					w.WriteHeader(http.StatusBadGateway)
				}
				if phase == "after_completed" {
					_, _ = fmt.Fprint(w, idleCompletedEvent)
				}
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var terminals int
			result, err := idleTestClient(t, 60*time.Millisecond).Execute(ctx, binding, Request{Protocol: "responses", Body: json.RawMessage(`{}`)}, func(event Event) error {
				if event.Type == "response.completed" {
					terminals++
				}
				return nil
			})
			var attempt *AttemptError
			if !errors.As(err, &attempt) || attempt.Code != "kernel_stream_idle_timeout" || attempt.SafeToRetry {
				t.Fatalf("idle must be an unreplayable read timeout: result=%+v error=%v", result, err)
			}
			if result.Completed || result.Settled || result.ClientDisconnect || !result.Dispatched || ctx.Err() != nil || terminals != 0 {
				t.Fatalf("idle falsely completed, settled or canceled caller: result=%+v ctx=%v terminals=%d", result, ctx.Err(), terminals)
			}
			if requests.Load() != 1 || cancels.Load() != 1 || cancelID.Load() != result.AttemptID || result.CancelStatus != "acknowledged" {
				t.Fatalf("single attempt cancellation lost: requests=%d cancels=%d result=%+v", requests.Load(), cancels.Load(), result)
			}
			if phase == "after_completed" && (!result.HasUsage || result.Terminal != "response.completed") {
				t.Fatalf("partial settlement metadata lost: %+v", result)
			}
		})
	}
}

func TestClientStreamIdleCountsByteFragmentsAndComments(t *testing.T) {
	for _, mode := range []string{"fragmented_event", "comments"} {
		t.Run(mode, func(t *testing.T) {
			binding := fakeKernel(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/internal/v1/cancel" {
					t.Error("active stream must not be canceled")
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.(http.Flusher).Flush()
				if mode == "comments" {
					for range 7 {
						_, _ = fmt.Fprint(w, ": internal heartbeat\n\n")
						w.(http.Flusher).Flush()
						time.Sleep(40 * time.Millisecond)
					}
					_, _ = fmt.Fprint(w, idleCompletedEvent)
					return
				}
				// No complete line is available until the final fragment. Raw bytes
				// must keep the read alive throughout this larger-than-idle event.
				for rest := idleCompletedEvent; rest != ""; {
					n := min(20, len(rest))
					_, _ = fmt.Fprint(w, rest[:n])
					w.(http.Flusher).Flush()
					rest = rest[n:]
					if rest != "" {
						time.Sleep(40 * time.Millisecond)
					}
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			var emitted []string
			result, err := idleTestClient(t, 200*time.Millisecond).Execute(ctx, binding, Request{Protocol: "responses", Body: json.RawMessage(`{}`)}, func(event Event) error {
				emitted = append(emitted, event.Type)
				return nil
			})
			if err != nil || !result.Completed || result.FirstTokenMs != nil || strings.Join(emitted, ",") != "response.completed" {
				t.Fatalf("real byte activity must survive without public heartbeats: result=%+v events=%v err=%v", result, emitted, err)
			}
		})
	}
}

func TestClientStreamIdleExcludesCallbackAndHeaderWait(t *testing.T) {
	for _, phase := range []string{"headers", "callback", "disabled"} {
		t.Run(phase, func(t *testing.T) {
			allowTail := make(chan struct{})
			binding := fakeKernel(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/internal/v1/cancel" {
					t.Error("non-reading work must not consume the idle allowance")
					return
				}
				if phase == "headers" {
					time.Sleep(250 * time.Millisecond)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.(http.Flusher).Flush()
				if phase == "callback" {
					_, _ = fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_idle\"}}\n\n")
					w.(http.Flusher).Flush()
					select {
					case <-allowTail:
					case <-r.Context().Done():
						return
					}
				}
				if phase == "disabled" {
					time.Sleep(250 * time.Millisecond)
				}
				_, _ = fmt.Fprint(w, idleCompletedEvent)
			})
			timeout := 100 * time.Millisecond
			if phase == "disabled" {
				timeout = 0
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			result, err := idleTestClient(t, timeout).Execute(ctx, binding, Request{Protocol: "responses", Body: json.RawMessage(`{}`)}, func(event Event) error {
				if phase == "callback" && event.Type == "response.created" {
					time.Sleep(250 * time.Millisecond)
					close(allowTail)
				}
				return nil
			})
			if err != nil || !result.Completed {
				t.Fatalf("phase %s must not be read-idle: result=%+v error=%v", phase, result, err)
			}
		})
	}
}

func TestClientStreamIdleKeepsCallerCancellationDistinct(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var cancels atomic.Int32
	binding := fakeKernel(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/cancel" {
			cancels.Add(1)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_idle\"}}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	result, err := idleTestClient(t, time.Second).Execute(ctx, binding, Request{Protocol: "responses", Body: json.RawMessage(`{}`)}, func(Event) error {
		cancel()
		return nil
	})
	var attempt *AttemptError
	if !errors.As(err, &attempt) || attempt.Code != "kernel_stream_incomplete" || attempt.SafeToRetry || !result.ClientDisconnect || result.Completed || cancels.Load() != 1 {
		t.Fatalf("caller cancel was reclassified as idle: result=%+v error=%v cancels=%d", result, err, cancels.Load())
	}
}
