package vmkernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func writeWrappedEvent(w http.ResponseWriter, event any) {
	data, _ := json.Marshal(event)
	fmt.Fprintf(w, "event: response\ndata: %s\n\n", data)
}

func TestClientWebSocketWrappedEvents(t *testing.T) {
	binding := fakeKernel(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/cancel" {
			t.Error("completed response must not be cancelled")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range []map[string]any{
			{"type": "codex.response.metadata", "headers": map[string]string{"x-codex-turn-state": "fixture-next", "x-codex-primary-used-percent": "12", "authorization": "private"}},
			{"type": "response.created", "response": map[string]any{"id": "resp_ws", "status": "in_progress"}},
			{"type": "response.output_text.delta", "delta": "OK"},
			{"type": "responsesapi.websocket_timing", "timing_metrics": map[string]int{"private": 1}},
			{"type": "response.completed", "response": map[string]any{"id": "resp_ws", "status": "completed", "usage": map[string]int{"input_tokens": 21, "output_tokens": 2}}},
		} {
			writeWrappedEvent(w, e)
		}
	})
	client := NewClient()
	defer client.Close()
	var events []string
	observed := false
	ctx := WithOnResponseHeaders(context.Background(), func(h http.Header) {
		if h.Get("Authorization") != "" {
			t.Error("private header escaped")
		}
		observed = observed || h.Get("X-Codex-Turn-State") == "fixture-next"
	})
	result, err := client.Execute(ctx, binding, Request{Protocol: "responses", Body: json.RawMessage("{}")}, func(e Event) error {
		if !observed {
			t.Error("metadata must precede public events")
		}
		events = append(events, e.Type)
		return nil
	})
	if err != nil || !result.Completed || !result.Settled || !result.HasUsage || result.ResponseID != "resp_ws" {
		t.Fatalf("wrapped completion lost: %+v %v", result, err)
	}
	if !reflect.DeepEqual(events, []string{CodexMetadataEvent, "response.created", "response.output_text.delta", "response.completed"}) {
		t.Fatalf("wrong public events: %v", events)
	}
	if result.Headers.Get("X-Codex-Primary-Used-Percent") != "12" || result.CancelStatus != "" {
		t.Fatalf("metadata/cancellation changed: %+v", result)
	}
}

func TestClientMetadataOnlyFailureDoesNotCommit(t *testing.T) {
	for _, source := range []string{"header", "event"} {
		t.Run(source, func(t *testing.T) {
			binding := fakeKernel(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if source == "header" {
					w.Header().Set("X-Codex-Turn-State", "unused-state")
				} else {
					writeWrappedEvent(w, map[string]any{"type": CodexMetadataEvent, "headers": map[string]string{"x-codex-turn-state": "unused-state"}})
				}
				writeWrappedEvent(w, map[string]any{"type": "error", "error": map[string]any{"code": "rate_limit_exceeded", "message": "limited"}})
			})
			client := NewClient()
			defer client.Close()
			body, _ := json.Marshal(map[string]string{"model": "gpt-5", "input": "hello"})
			result, err := client.Execute(context.Background(), binding, Request{Protocol: "responses", Body: body}, func(Event) error {
				t.Error("metadata must not commit a rejected attempt")
				return nil
			})
			var attempt *AttemptError
			if !errors.As(err, &attempt) || attempt.Code != "kernel_response_failed" || !result.Settled || result.Completed || result.FirstTokenMs != nil {
				t.Fatalf("metadata changed failure semantics: %+v %v", result, err)
			}
		})
	}
}

func TestClientWebSocketWrappedFailures(t *testing.T) {
	for _, name := range []string{"upstream_error", "failed", "missing_type", "missing_usage", "unknown_type"} {
		t.Run(name, func(t *testing.T) {
			response := map[string]any{"id": "resp_bad", "status": "completed", "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}}
			event := map[string]any{"type": "response.completed", "response": response}
			code, settled := "kernel_completion_metadata_missing", false
			providerError := map[string]string{"code": "rate_limit_exceeded", "message": "Rate limited"}
			switch name {
			case "upstream_error":
				event = map[string]any{"type": "error", "error": providerError}
				code = "kernel_response_failed"
				settled = true
			case "failed":
				event["type"] = "response.failed"
				response["status"] = "failed"
				response["error"] = providerError
				code = "kernel_response_failed"
				settled = true
			case "missing_type":
				delete(event, "type")
			case "missing_usage":
				delete(response, "usage")
				settled = true
			case "unknown_type":
				event["type"] = "private.completed"
			}
			binding := fakeKernel(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/internal/v1/cancel" {
					w.WriteHeader(204)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				writeWrappedEvent(w, event)
			})
			client := NewClient()
			defer client.Close()
			result, err := client.Execute(context.Background(), binding, Request{Protocol: "responses", Body: json.RawMessage("{}")}, func(e Event) error { t.Errorf("invalid terminal escaped: %s", e.Type); return nil })
			var attempt *AttemptError
			if !errors.As(err, &attempt) || attempt.Code != code || result.Completed || result.Settled != settled {
				t.Fatalf("incorrect failure: %+v %v", result, err)
			}
			if code == "kernel_response_failed" && !strings.Contains(string(result.ErrorEvent), "rate_limit_exceeded") {
				t.Fatal("upstream error classification lost")
			}
		})
	}
}
