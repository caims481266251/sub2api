package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

func TestVMKernelWSReplayOwnershipAndToolContext(t *testing.T) {
	h := &vmKernelWSHistory{}
	if _, err := h.prepare([]byte(`{"type":"response.create","previous_response_id":"foreign","input":[]}`)); err == nil {
		t.Fatal("accepted foreign response")
	}
	first, err := h.prepare([]byte(`{"type":"response.create","model":"gpt-test","input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = h.complete(first, []byte(`{"id":"resp_owned","output":[{"type":"function_call","call_id":"call1","name":"lookup","arguments":"{}"}]}`)); err != nil {
		t.Fatal(err)
	}
	next, err := h.prepare([]byte(`{"type":"response.create","model":"gpt-test","previous_response_id":"resp_owned","input":[{"type":"function_call_output","call_id":"call1","output":"ok"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Input    []json.RawMessage `json:"input"`
		Previous string            `json:"previous_response_id"`
	}
	if err = json.Unmarshal(next, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Input) != 3 || result.Previous != "" {
		t.Fatalf("invalid replay: %s", next)
	}
}

func TestVMKernelWSHistoryBoundedSnapshotsAndSemantics(t *testing.T) {
	h := &vmKernelWSHistory{}
	body := []byte(`{"model":"gpt-test","instructions":"rules","tools":[],"input":[]}`)
	for i := 0; i < 33; i++ {
		response := fmt.Sprintf(`{"id":"resp_%d","output":[]}`, i)
		if err := h.complete(body, []byte(response)); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.snapshots) != 32 {
		t.Fatalf("snapshots %d", len(h.snapshots))
	}
	if _, err := h.prepare([]byte(`{"type":"response.create","previous_response_id":"resp_0","input":[]}`)); err == nil {
		t.Fatal("evicted id accepted")
	}
	for _, field := range []string{`"model":"other"`, `"instructions":"changed"`, `"tools":[{"type":"function","name":"new"}]`} {
		if _, err := h.prepare([]byte(`{"type":"response.create","previous_response_id":"resp_1","input":[],` + field + `}`)); err != nil {
			t.Fatalf("per-turn request options rejected: %s: %v", field, err)
		}
	}
	body, err := h.prepare([]byte(`{"type":"response.create","previous_response_id":"resp_1","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(`"instructions"`)) {
		t.Fatalf("previous instructions leaked into the next turn: %s", body)
	}
	if !bytes.Contains(body, []byte(`"model":"gpt-test"`)) {
		t.Fatalf("missing model fallback: %s", body)
	}
}
