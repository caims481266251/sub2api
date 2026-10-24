package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestVMKernelToolsNativeTerminalPreservesAllItems(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[stream], func(t *testing.T) {
			var emitted []byte
			p := newVMKernelPresentation("responses", "responses", stream, func(_ string, data []byte) error {
				if gjson.GetBytes(data, "type").String() == "response.completed" {
					emitted = append([]byte(nil), data...)
				}
				return nil
			})
			for _, event := range vmToolEvents("patch", "functions", "resp_complete_items") {
				if gjson.Get(event, "type").String() == "response.completed" {
					var err error
					event, err = sjson.SetRaw(event, "response.output", "[]")
					require.NoError(t, err)
				}
				require.NoError(t, p.event("", []byte(event)))
			}
			out, err := p.finish("gpt-6-luna")
			require.NoError(t, err)
			if stream {
				out = []byte(gjson.GetBytes(emitted, "response").Raw)
			}
			require.Len(t, gjson.GetBytes(out, "output").Array(), 3)
			require.Equal(t, "patch", gjson.GetBytes(out, "output.0.name").String())
			require.Equal(t, "lookup", gjson.GetBytes(out, "output.1.name").String())
			require.Equal(t, "tool_search_call", gjson.GetBytes(out, "output.2.type").String())
			require.Equal(t, "9007199254740993", gjson.GetBytes(out, "output.2.opaque").Raw)
			require.True(t, gjson.GetBytes(out, "provider_extension.value").Bool())
			require.JSONEq(t, string(out), string(p.nativeResponse))
		})
	}
}

func TestVMKernelToolsHeadersReachKernelWithoutCredentials(t *testing.T) {
	for _, model := range []string{"gpt-6-luna", "gpt-5.5"} {
		t.Run(model, func(t *testing.T) {
			runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
				var envelope struct{ Headers map[string]string }
				require.NoError(t, json.NewDecoder(r.Body).Decode(&envelope))
				require.Equal(t, "responses=experimental", envelope.Headers["openai-beta"])
				wantLite := "true"
				if model == "gpt-5.5" {
					wantLite = ""
				}
				require.Equal(t, wantLite, envelope.Headers[strings.ToLower(responsesLiteHeader)])
				require.Empty(t, envelope.Headers["authorization"])
				require.Empty(t, envelope.Headers["cookie"])
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "resp_headers", "status": "completed", "output": []any{}, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}})
			})
			c, _ := vmToolContext("/v1/responses")
			c.Request.Header.Set("OpenAI-Beta", "responses=experimental")
			c.Request.Header.Set(responsesLiteHeader, "true")
			c.Request.Header.Set("Authorization", "Bearer dummy-client-secret")
			c.Request.Header.Set("Cookie", "dummy-cookie")
			body, err := json.Marshal(map[string]any{"model": model, "input": "hello"})
			require.NoError(t, err)
			result, err := vmKernelHTTP(context.Background(), runtime, c, account, "responses", "responses", body, false, model)
			require.NoError(t, err)
			require.True(t, result.Completed)
		})
	}
}

func TestVMKernelToolsNamespaceCollisionNeverDispatches(t *testing.T) {
	c, w := vmToolContext("/v1/responses")
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, ExecutionMode: AccountExecutionModeVMKernel, Extra: map[string]any{"openai_responses_flatten_namespaces": true}}
	body, err := sjson.SetBytes([]byte(vmToolsRequest), "tools.-1", map[string]any{"type": "function", "name": "functions__lookup", "parameters": map[string]string{"type": "object"}})
	require.NoError(t, err)
	_, err = (&OpenAIGatewayService{}).forwardVMKernelResponses(context.Background(), c, account, body, "responses", "")
	require.ErrorContains(t, err, "conflicts")
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Equal(t, "invalid_request_error", gjson.Get(w.Body.String(), "error.type").String())
}

func TestVMKernelToolsLiteKeepsNativeHistoryWithFlattenEnabled(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, ExecutionMode: AccountExecutionModeVMKernel, Extra: map[string]any{"openai_responses_flatten_namespaces": true}}
	c, _ := vmToolContext("/v1/responses")
	c.Request.Header.Set(responsesLiteHeader, "true")
	body, err := sjson.SetBytes([]byte(vmToolsRequest), "input.-1", map[string]any{"type": "custom_tool_call", "name": "patch", "namespace": "functions", "call_id": "call_previous", "input": "patch text"})
	require.NoError(t, err)
	prepared, _, _, err := (&OpenAIGatewayService{}).prepareVMKernelResponses(context.Background(), c, account, body, "responses", "")
	require.NoError(t, err)
	var payload struct{ Input []map[string]any }
	require.NoError(t, json.Unmarshal(prepared, &payload))
	var call map[string]any
	for _, item := range payload.Input {
		if item["type"] == "custom_tool_call" {
			call = item
		}
	}
	require.NotNil(t, call)
	require.Equal(t, "patch", call["name"])
	require.Equal(t, "functions", call["namespace"])
	require.Empty(t, openAIResponsesNamespaceNames(c))
}

func TestVMKernelToolsHTTPContinuationKeepsCompleteOutput(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, flatten := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream_%v_flatten_%v", stream, flatten), func(t *testing.T) {
				var calls atomic.Int32
				responseID := fmt.Sprintf("resp_http_tools_%v_%v", stream, flatten)
				runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
					n := calls.Add(1)
					var envelope struct{ Body json.RawMessage }
					if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
						t.Error(err)
						return
					}
					name, namespace := "patch", "functions"
					if flatten {
						name, namespace = "functions__patch", ""
					}
					if n == 2 {
						found := map[string]bool{}
						for _, item := range gjson.GetBytes(envelope.Body, "input").Array() {
							kind := item.Get("type").String()
							found[kind] = true
							if kind == "custom_tool_call" && (item.Get("name").String() != name || item.Get("namespace").String() != namespace) {
								t.Error("replayed custom identity changed")
							}
						}
						for _, kind := range []string{"custom_tool_call", "function_call", "tool_search_call", "custom_tool_call_output"} {
							if !found[kind] {
								t.Errorf("replay lost %s", kind)
							}
						}
					}
					w.Header().Set("Content-Type", "text/event-stream")
					for _, event := range vmToolEvents(name, namespace, fmt.Sprintf("%s_%d", responseID, n)) {
						if gjson.Get(event, "type").String() == "response.completed" {
							event, _ = sjson.SetRaw(event, "response.output", "[]")
						}
						_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
					}
				}, func(a *Account) { a.Extra = map[string]any{"openai_responses_flatten_namespaces": flatten} })
				svc := &OpenAIGatewayService{vmKernel: runtime}
				t.Cleanup(func() {
					vmKernelContinuations.mu.Lock()
					defer vmKernelContinuations.mu.Unlock()
					for n := 1; n <= 2; n++ {
						key := vmKernelContinuationKey{groupID: 998732, userID: 71, accountID: account.ID, responseID: fmt.Sprintf("%s_%d", responseID, n)}
						entry := vmKernelContinuations.entries[key]
						vmKernelContinuations.bytes -= entry.snapshot.size
						delete(vmKernelContinuations.entries, key)
					}
					vmKernelContinuations.prune(time.Now())
				})
				body, err := sjson.SetBytes([]byte(vmToolsRequest), "stream", stream)
				require.NoError(t, err)
				result, err := svc.forwardVMKernelResponses(context.Background(), vmContinuationContext(71, 81, 998732), account, body, "responses", "")
				require.NoError(t, err)
				require.True(t, result.VMKernelUsageAuthoritative)
				next, err := sjson.SetBytes(body, "previous_response_id", responseID+"_1")
				require.NoError(t, err)
				next, err = sjson.SetBytes(next, "input", []any{map[string]any{"type": "custom_tool_call_output", "call_id": "call_fixture", "output": "done"}})
				require.NoError(t, err)
				result, err = svc.forwardVMKernelResponses(context.Background(), vmContinuationContext(71, 81, 998732), account, next, "responses", "")
				require.NoError(t, err)
				require.True(t, result.VMKernelUsageAuthoritative)
				require.EqualValues(t, 2, calls.Load())
			})
		}
	}
}
