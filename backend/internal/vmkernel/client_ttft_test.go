package vmkernel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func TestClientFirstTokenPredicateIsRequestScoped(t *testing.T) {
	binding := fakeKernel(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"response.created","response":{"id":"resp_ttft","status":"in_progress"}}`,
			`{"type":"response.output_item.added","item":{"type":"message","content":[]}}`,
			`{"type":"response.output_text.delta","delta":"hello"}`,
			`{"type":"response.completed","response":{"id":"resp_ttft","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
	})
	client := NewClient()
	defer client.Close()
	var checked []string
	semantic := func(event Event) bool {
		checked = append(checked, event.Type)
		return event.Type == "response.output_item.added"
	}
	for _, tc := range []struct {
		name      string
		predicate func(Event) bool
		wantToken bool
	}{
		{name: "semantic", predicate: semantic, wantToken: true},
		{name: "no_matching_output", predicate: func(Event) bool { return false }},
		{name: "default_after_override", wantToken: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := WithFirstTokenPredicate(context.Background(), tc.predicate)
			result, err := client.Execute(ctx, binding, Request{Protocol: "responses", Body: json.RawMessage(`{}`)}, nil)
			if err != nil || !result.Completed {
				t.Fatalf("attempt failed: %+v %v", result, err)
			}
			if (result.FirstTokenMs != nil) != tc.wantToken {
				t.Fatalf("unexpected first token: %+v", result.FirstTokenMs)
			}
		})
	}
	if len(checked) != 2 || checked[0] != "response.created" || checked[1] != "response.output_item.added" {
		t.Fatalf("first token must be selected once and not leak to other requests: %v", checked)
	}
}
