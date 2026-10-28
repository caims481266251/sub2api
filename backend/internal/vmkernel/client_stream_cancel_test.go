package vmkernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// The runtime declares this contract for the pinned Codex kernel. A request's
// protocol or a past control-route response must never select it implicitly.
func TestClientStreamCloseCancellationBoundaries(t *testing.T) {
	for _, phase := range []string{"before_headers", "sse_read", "error_body", "downstream_emit", "idle_sse", "idle_error"} {
		t.Run(phase, func(t *testing.T) {
			started, streamClosed := make(chan struct{}), make(chan struct{})
			var controls atomic.Int32
			binding := fakeKernel(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/internal/v1/cancel" {
					controls.Add(1)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if phase != "before_headers" {
					w.Header().Set("Content-Type", "text/event-stream")
					if phase == "error_body" || phase == "idle_error" {
						w.WriteHeader(http.StatusBadGateway)
					}
					if phase == "downstream_emit" {
						fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"fixture\"}\n\n")
					}
					w.(http.Flusher).Flush()
				}
				close(started)
				<-r.Context().Done()
				close(streamClosed)
			})
			binding.CancellationMode = CancellationModeStreamClose
			client := NewClient()
			defer client.Close()
			if phase == "idle_sse" || phase == "idle_error" {
				client.StreamDataIntervalTimeout = 75 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			emitErr := errors.New("fixture downstream write failed")
			type outcome struct {
				result *Result
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := client.Execute(ctx, binding, Request{Protocol: "responses", Body: json.RawMessage(`{}`)}, func(Event) error {
					if phase == "downstream_emit" {
						return emitErr
					}
					return nil
				})
				done <- outcome{result, err}
			}()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("execution did not reach kernel")
			}
			callerCanceled := phase == "before_headers" || phase == "sse_read" || phase == "error_body"
			if callerCanceled {
				cancel()
			}
			var got outcome
			select {
			case got = <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("request cancellation waited for an unsupported control API")
			}
			select {
			case <-streamClosed:
			case <-time.After(time.Second):
				t.Fatal("kernel did not observe execution-stream closure")
			}
			var attempt *AttemptError
			if !errors.As(got.err, &attempt) || attempt.SafeToRetry || got.result == nil || !got.result.Dispatched ||
				got.result.Completed || got.result.Settled || !got.result.ExecutionTransportCanceled ||
				got.result.CancelStatus != "stream_closed" || got.result.CancelHTTPStatus != 0 || controls.Load() != 0 {
				t.Fatalf("stream-close invented settlement, replay, or control support: result=%+v err=%v controls=%d", got.result, got.err, controls.Load())
			}
			if got.result.ClientDisconnect != callerCanceled {
				t.Fatalf("caller cancellation misclassified: result=%+v err=%v", got.result, got.err)
			}
			if callerCanceled && !errors.Is(got.err, context.Canceled) {
				t.Fatalf("caller cause lost in phase %s: %v", phase, got.err)
			}
			if phase == "downstream_emit" && !errors.Is(got.err, emitErr) {
				t.Fatalf("downstream error lost: %v", got.err)
			}
			if (phase == "idle_sse" || phase == "idle_error") && (attempt.Code != "kernel_stream_idle_timeout" || !errors.Is(got.err, errKernelStreamIdle)) {
				t.Fatalf("read idle was mislabeled as caller cancellation: %v", got.err)
			}
		})
	}
}

func TestClientStreamCloseDoesNotHideUnexpectedEOF(t *testing.T) {
	var controls atomic.Int32
	binding := fakeKernel(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/cancel" {
			controls.Add(1)
			return
		}
		conn, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		fmt.Fprint(buffer, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: 1000\r\n\r\ndata: {\"type\":\"response.created\"}\n\n")
		_ = buffer.Flush()
	})
	binding.CancellationMode = CancellationModeStreamClose
	client := NewClient()
	defer client.Close()
	result, err := client.Execute(context.Background(), binding, Request{Protocol: "responses", Body: json.RawMessage(`{}`)}, nil)
	var attempt *AttemptError
	if !errors.Is(err, io.ErrUnexpectedEOF) || !errors.As(err, &attempt) || attempt.SafeToRetry || result.ClientDisconnect ||
		result.Settled || result.Completed || !result.ExecutionTransportCanceled || result.CancelStatus != "stream_closed" || controls.Load() != 0 {
		t.Fatalf("upstream EOF was hidden by local cleanup: result=%+v err=%v controls=%d", result, err, controls.Load())
	}
}

func TestClientStreamClosePreservesConcurrentAndPooledRequests(t *testing.T) {
	canceledStarted, partnerStarted, canceledClosed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	releasePartner := make(chan struct{})
	var requests atomic.Int32
	var partnerCanceled atomic.Bool
	binding, connections, _ := pooledKernel(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/codex/responses" {
			t.Error("unexpected execution endpoint")
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		switch requests.Add(1) {
		case 1:
			close(canceledStarted)
			<-r.Context().Done()
			close(canceledClosed)
		case 2:
			close(partnerStarted)
			select {
			case <-releasePartner:
				completePoolRequest(w, r)
			case <-r.Context().Done():
				partnerCanceled.Store(true)
			}
		default:
			completePoolRequest(w, r)
		}
	})
	binding.CancellationMode = CancellationModeStreamClose
	client := NewClient()
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		result *Result
		err    error
	}
	canceledDone := make(chan outcome, 1)
	go func() {
		result, err := client.Execute(ctx, binding, Request{Protocol: "responses", Body: json.RawMessage(`{}`)}, nil)
		canceledDone <- outcome{result, err}
	}()
	select {
	case <-canceledStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first request not started")
	}
	partnerDone := make(chan error, 1)
	go func() { partnerDone <- executePoolRequest(client, binding) }()
	select {
	case <-partnerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("partner request not concurrent")
	}
	cancel()
	select {
	case got := <-canceledDone:
		var attempt *AttemptError
		if !errors.Is(got.err, context.Canceled) || !errors.As(got.err, &attempt) || attempt.SafeToRetry ||
			got.result.Settled || got.result.Completed || !got.result.ClientDisconnect || !got.result.ExecutionTransportCanceled || got.result.CancelStatus != "stream_closed" {
			t.Fatalf("canceled request lost its outcome: %+v %v", got.result, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled request did not finish")
	}
	select {
	case <-canceledClosed:
	case <-time.After(time.Second):
		t.Fatal("canceled request's transport remained open")
	}
	close(releasePartner)
	if err := <-partnerDone; err != nil {
		t.Fatalf("partner did not complete: %v", err)
	}
	for range 3 {
		if err := executePoolRequest(client, binding); err != nil {
			t.Fatalf("subsequent pooled request failed: %v", err)
		}
	}
	if requests.Load() != 5 || connections.Load() != 2 || partnerCanceled.Load() {
		t.Fatalf("request cleanup disrupted shared pool: requests=%d connections=%d partner_canceled=%t", requests.Load(), connections.Load(), partnerCanceled.Load())
	}
}

func TestClientCancellationModeMustBeTrustedAndKnown(t *testing.T) {
	var dispatched atomic.Int32
	binding := fakeKernel(t, func(w http.ResponseWriter, r *http.Request) { dispatched.Add(1) })
	binding.CancellationMode = CancellationMode("unknown")
	client := NewClient()
	defer client.Close()
	result, err := client.Execute(context.Background(), binding, Request{Protocol: "responses", Body: json.RawMessage(`{}`)}, nil)
	var attempt *AttemptError
	if !errors.As(err, &attempt) || attempt.Code != "kernel_binding_invalid" || !attempt.SafeToRetry || result != nil || dispatched.Load() != 0 {
		t.Fatalf("unrecognized trusted contract was dispatched: result=%+v err=%v", result, err)
	}
}
