package service

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestVMKernelConversionMatrix(t *testing.T) {
	cases := []struct{ ingress, provider, body string }{
		{"chat", "responses", `{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`},
		{"messages", "responses", `{"model":"gpt-test","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`},
		{"chat", "messages", `{"model":"claude-test","messages":[{"role":"user","content":"hello"}]}`},
		{"responses", "messages", `{"model":"claude-test","input":"hello","max_output_tokens":100}`},
	}
	for _, tc := range cases {
		t.Run(tc.ingress+"_"+tc.provider, func(t *testing.T) {
			b, err := vmKernelConvertRequest(nil, tc.ingress, tc.provider, []byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			var obj map[string]json.RawMessage
			if err = json.Unmarshal(b, &obj); err != nil {
				t.Fatal(err)
			}
			key := "input"
			if tc.provider == "messages" {
				key = "messages"
			}
			if len(obj[key]) == 0 {
				t.Fatalf("missing %s", key)
			}
		})
	}
}

func TestVMKernelNativeBufferedPreservesThinkingAndToolFragments(t *testing.T) {
	p := newVMKernelPresentation("messages", "messages", false, nil)
	events := []string{
		`{"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","model":"claude-test","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"thought"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"signed"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tool1","name":"lookup","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"ok\"}"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`,
	}
	for _, e := range events {
		if err := p.event("", []byte(e)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.finish("claude-test"); err == nil {
		t.Fatal("missing terminal accepted")
	}
	if err := p.event("message_stop", []byte(`{"type":"message_stop"}`)); err != nil {
		t.Fatal(err)
	}
	b, err := p.finish("claude-test")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"signature":"signed"`) || !strings.Contains(string(b), `"q":"ok"`) {
		t.Fatalf("lost native content: %s", b)
	}
}

func TestVMKernelConversionDoesNotFinalizeMissingTerminal(t *testing.T) {
	p := newVMKernelPresentation("chat", "responses", false, nil)
	if err := p.event("response.output_text.delta", []byte(`{"type":"response.output_text.delta","delta":"partial"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.finish("gpt-test"); err == nil {
		t.Fatal("partial synthesized as completed")
	}
}

func TestVMKernelTerminalWaitsForAuthoritativeTail(t *testing.T) {
	var output []string
	p := newVMKernelPresentation("chat", "responses", true, func(_ string, data []byte) error { output = append(output, string(data)); return nil })
	for _, raw := range []string{
		`{"type":"response.created","response":{"id":"r1","model":"gpt-test","status":"in_progress"}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hello"}`,
		`{"type":"response.completed","response":{"id":"r1","model":"gpt-test","status":"completed","output":[],"usage":{"input_tokens":0,"output_tokens":0}}}`,
	} {
		if err := p.event("", []byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	before := strings.Join(output, "\n")
	if strings.Contains(before, `"finish_reason":"stop"`) || strings.Contains(before, "[DONE]") {
		t.Fatalf("premature completion: %s", before)
	}
	if _, err := p.finish("gpt-test", json.RawMessage(`{"input_tokens":7,"output_tokens":3,"input_tokens_details":{"cached_tokens":2}}`)); err != nil {
		t.Fatal(err)
	}
	after := strings.Join(output, "\n")
	if !strings.Contains(after, "[DONE]") {
		t.Fatalf("missing completed delimiter: %s", after)
	}
	usageChunks := 0
	for i, raw := range output {
		if raw == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []json.RawMessage `json:"choices"`
			Usage   *struct {
				Prompt     int `json:"prompt_tokens"`
				Completion int `json:"completion_tokens"`
				Details    struct {
					Cached int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
			t.Fatal(err)
		}
		if chunk.Usage == nil {
			continue
		}
		usageChunks++
		if len(chunk.Choices) != 0 || chunk.Usage.Prompt != 7 || chunk.Usage.Completion != 3 || chunk.Usage.Details.Cached != 2 {
			t.Fatalf("missing authoritative chat usage: %s", raw)
		}
		if i != len(output)-2 || output[i+1] != "[DONE]" {
			t.Fatal("usage must precede the final delimiter")
		}
	}
	if usageChunks != 1 {
		t.Fatalf("got %d usage chunks, want one", usageChunks)
	}
}

func TestVMKernelBufferedLimitAndInvalidToolFragments(t *testing.T) {
	p := newVMKernelPresentation("chat", "responses", false, nil)
	if err := p.event("", []byte(strings.Repeat("x", vmKernelPresentationLimit+1))); err == nil {
		t.Fatal("oversize event accepted")
	}
	p = newVMKernelPresentation("messages", "messages", false, nil)
	for _, raw := range []string{
		`{"type":"message_start","message":{"id":"m","content":[]}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t","name":"tool","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{"}}`,
	} {
		if err := p.event("", []byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.event("", []byte(`{"type":"content_block_stop","index":0}`)); err == nil {
		t.Fatal("malformed tool JSON accepted")
	}
}

func TestVMKernelPresentationTwelveHTTPShapes(t *testing.T) {
	messages := []string{
		`{"type":"message_start","message":{"id":"msg1","type":"message","role":"assistant","model":"claude-test","content":[],"usage":{"input_tokens":2,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		`{"type":"message_stop"}`,
	}
	responses := []string{
		`{"type":"response.created","response":{"id":"resp1","object":"response","status":"in_progress","model":"gpt-test","output":[]}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"item1","type":"message","role":"assistant","content":[]}}`,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hello"}`,
		`{"type":"response.completed","response":{"id":"resp1","object":"response","status":"completed","model":"gpt-test","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":2,"output_tokens":3}}}`,
	}
	for _, provider := range []string{"messages", "responses"} {
		for _, ingress := range []string{"messages", "responses", "chat"} {
			for _, stream := range []bool{false, true} {
				name := provider + "_" + ingress
				if stream {
					name += "_stream"
				} else {
					name += "_json"
				}
				t.Run(name, func(t *testing.T) {
					var emitted []string
					p := newVMKernelPresentation(ingress, provider, stream, func(_ string, data []byte) error { emitted = append(emitted, string(data)); return nil })
					events := responses
					if provider == "messages" {
						events = messages
					}
					for _, event := range events {
						if err := p.event("", []byte(event)); err != nil {
							t.Fatal(err)
						}
					}
					body, err := p.finish("client-model", json.RawMessage(`{"input_tokens":2,"output_tokens":3}`))
					if err != nil {
						t.Fatal(err)
					}
					if stream {
						if !strings.Contains(strings.Join(emitted, "\n"), "hello") {
							t.Fatalf("missing streamed text: %v", emitted)
						}
					} else {
						if !json.Valid(body) || !strings.Contains(string(body), "hello") {
							t.Fatalf("invalid buffered result: %s", body)
						}
					}
				})
			}
		}
	}
}
