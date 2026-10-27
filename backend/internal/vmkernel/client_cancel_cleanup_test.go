package vmkernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// The installed kernel may lack /cancel; closing the execution stream must
// happen before waiting for that independent control request, even if the
// caller's context itself is still live (for example a downstream write error).
func TestClientClosesExecutionBeforeWaitingForCancel(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusNotFound} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			streamClosed := make(chan struct{})
			cancelStarted := make(chan struct{})
			releaseControl := make(chan struct{}, 1)
			defer close(releaseControl)
			binding := fakeKernel(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/internal/v1/cancel" {
					close(cancelStarted)
					select {
					case <-releaseControl:
					case <-r.Context().Done():
						return
					}
					w.WriteHeader(status)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"text\"}\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(streamClosed)
			})
			client := NewClient()
			defer client.Close()
			client.CancelTimeout = 2 * time.Second
			client.StreamDataIntervalTimeout = 100 * time.Millisecond
			done := make(chan struct{})
			var result *Result
			var err error
			go func() {
				defer close(done)
				result, err = client.Execute(context.Background(), binding, Request{Protocol: "responses", Body: json.RawMessage(`{}`)}, func(Event) error {
					return errors.New("downstream disconnected")
				})
			}()
			select {
			case <-cancelStarted:
			case <-time.After(time.Second):
				t.Fatal("control request did not start")
			}
			select {
			case <-streamClosed:
			case <-time.After(time.Second):
				t.Fatal("execution still open while control request is blocked")
			}
			// The control request is deliberately still pending here. The original
			// stream has already closed, without a successful /cancel response.
			releaseControl <- struct{}{}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("execution did not finish")
			}
			want := "acknowledged"
			if status == http.StatusNotFound {
				want = "unavailable"
			}
			var attempt *AttemptError
			if !errors.As(err, &attempt) || attempt.SafeToRetry || result.Completed || result.Settled || result.CancelStatus != want || result.CancelHTTPStatus != status {
				t.Fatalf("incorrect cancellation result: %+v %v", result, err)
			}
		})
	}
}

func TestClientIdleTimeoutClosesExecutionWithUnavailableCancel(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadGateway} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			streamClosed := make(chan struct{})
			binding := fakeKernel(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/internal/v1/cancel" {
					// The original stream must close independently of this unsupported
					// control route, including when reading a stalled error body.
					select {
					case <-streamClosed:
					case <-time.After(time.Second):
						t.Error("idle execution remained open while cancel was pending")
					}
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.WriteHeader(status)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(streamClosed)
			})
			client := idleTestClient(t, 60*time.Millisecond)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			result, err := client.Execute(ctx, binding, Request{Protocol: "responses", Body: json.RawMessage(`{}`)}, nil)
			var attempt *AttemptError
			if !errors.As(err, &attempt) || attempt.Code != "kernel_stream_idle_timeout" || attempt.SafeToRetry ||
				result.Completed || result.Settled || result.ClientDisconnect || ctx.Err() != nil ||
				result.CancelStatus != "unavailable" || result.CancelHTTPStatus != http.StatusNotFound {
				t.Fatalf("idle timeout lost cancellation compatibility: result=%+v err=%v", result, err)
			}
		})
	}
}
