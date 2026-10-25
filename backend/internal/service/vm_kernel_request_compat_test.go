package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Match the production request size and verify the whole tool result reaches
// the kernel socket, including for clients requesting a non-streaming response.
func TestVMKernelForwardLargeRequestReachesKernel(t *testing.T) {
	const requestBytes = 36038816
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream_%v", stream), func(t *testing.T) {
			prefix := fmt.Sprintf(`{"model":"gpt-6.1-sol","stream":%v,"input":[{"type":"function_call","name":"lookup","call_id":"call_large","arguments":"{}"},{"type":"function_call_output","call_id":"call_large","output":"`, stream)
			const suffix = `"}]}`
			output := "BEGIN-TOOL-RESULT:" + strings.Repeat("x", requestBytes-len(prefix)-len(suffix)-len("BEGIN-TOOL-RESULT::END-TOOL-RESULT")) + ":END-TOOL-RESULT"
			body := []byte(prefix + output + suffix)
			require.Len(t, body, requestBytes)
			wantHash := sha256.Sum256([]byte(output))
			var attempts atomic.Int32
			runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				var envelope struct{ Body json.RawMessage }
				if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				got := gjson.GetBytes(envelope.Body, `input.#(type=="function_call_output")`)
				if got.Get("call_id").String() != "fc_large" || sha256.Sum256([]byte(got.Get("output").String())) != wantHash {
					t.Error("large tool result was truncated, dropped, or lost its call pairing")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				for _, event := range vmForwardEvents("responses") {
					_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
				}
			})
			c, w := vmToolContext("/v1/responses")
			result, err := (&OpenAIGatewayService{vmKernel: runtime}).forwardVMKernelResponses(context.Background(), c, account, body, "responses", "")
			require.NoError(t, err)
			require.NotNil(t, result)
			require.True(t, result.VMKernelUsageAuthoritative)
			require.Equal(t, stream, result.Stream)
			require.EqualValues(t, 1, attempts.Load())
			require.Equal(t, http.StatusOK, w.Code)
			require.Contains(t, w.Body.String(), "hello")
		})
	}
}

// Compare the payloads actually sent by ordinary OAuth and Docker forwarding.
// The expected outputs also guard against both paths silently losing valid data.
func TestVMKernelToolOutputCompatibilityMatchesOrdinary(t *testing.T) {
	for _, tc := range []struct {
		name    string
		input   string
		outputs []string
	}{
		{
			name: "valid pair and orphan",
			input: `[{"type":"function_call","call_id":"call_keep","name":"lookup","arguments":"{}"},
				{"type":"function_call_output","call_id":"call_keep","output":"valid result"},
				{"type":"function_call_output","call_id":"fco_missing","output":"orphan result"}]`,
			outputs: []string{"valid result"},
		},
		{
			name: "normalize IDs before matching",
			input: `[{"type":"function_call","call_id":"call_same","name":"lookup","arguments":"{}"},
				{"type":"function_call_output","call_id":"fc_same","output":"normalized pair"}]`,
			outputs: []string{"normalized pair"},
		},
		{
			name: "fco prefix is a valid paired ID",
			input: `[{"type":"function_call","call_id":"fco_existing","name":"lookup","arguments":"{}"},
				{"type":"function_call_output","call_id":"fco_existing","output":"fco pair"}]`,
			outputs: []string{"fco pair"},
		},
		{
			name: "reference and orphan",
			input: `[{"type":"item_reference","id":"call_reference"},
				{"type":"function_call_output","call_id":"call_reference","output":"referenced result"},
				{"type":"function_call_output","call_id":"call_missing","output":"orphan result"}]`,
			outputs: []string{"referenced result"},
		},
		{
			name: "named standalone delegation",
			input: `[{"type":"function_call_output","name":"send_message_to_thread","namespace":"codex_app","output":"delegated task"},
				{"type":"function_call_output","call_id":"call_missing","output":"orphan result"}]`,
			outputs: []string{"delegated task"},
		},
		{
			name: "custom search and MCP pairs",
			input: `[{"type":"custom_tool_call","call_id":"call_patch","name":"patch","input":"patch text"},
				{"type":"custom_tool_call_output","call_id":"call_patch","output":"patched"},
				{"type":"tool_search_call","call_id":"call_search","arguments":{}},
				{"type":"tool_search_output","call_id":"call_search","output":"found"},
				{"type":"mcp_tool_call","call_id":"call_mcp","name":"lookup","arguments":"{}"},
				{"type":"mcp_tool_call_output","call_id":"call_mcp","output":"MCP result"},
				{"type":"custom_tool_call_output","call_id":"call_missing","output":"orphan result"}]`,
			outputs: []string{"patched", "found", "MCP result"},
		},
		{
			name: "outputs alone cannot establish a call",
			input: `[{"type":"function_call_output","call_id":"call_missing","output":"orphan one"},
				{"type":"function_call_output","call_id":"call_missing","output":"orphan two"}]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.6-sol","stream":false,"input":` + tc.input + `}`)
			upstream := &httpUpstreamRecorder{responses: []*http.Response{
				newOpenAIRejectedFieldTestResponse(http.StatusOK, `{"id":"resp_ordinary","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`),
			}}
			_, err := newOpenAIRejectedFieldTestService(upstream).Forward(context.Background(), newOpenAIRejectedFieldTestContext(body), newOpenAIOAuthNamespaceTestAccount(), body)
			require.NoError(t, err)
			require.Len(t, upstream.bodies, 1)

			received := make(chan json.RawMessage, 1)
			runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
				var envelope struct{ Body json.RawMessage }
				if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				received <- envelope.Body
				w.Header().Set("Content-Type", "text/event-stream")
				for _, event := range vmForwardEvents("responses") {
					_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
				}
			})
			result, err := (&OpenAIGatewayService{vmKernel: runtime}).forwardVMKernelResponses(context.Background(), newOpenAIRejectedFieldTestContext(body), account, body, "responses", "")
			require.NoError(t, err)
			require.NotNil(t, result)
			require.True(t, result.VMKernelUsageAuthoritative)
			got := <-received
			require.JSONEq(t, gjson.GetBytes(upstream.bodies[0], "input").Raw, gjson.GetBytes(got, "input").Raw)
			var outputs []string
			for _, item := range gjson.GetBytes(got, "input").Array() {
				if isCodexToolCallOutputItemType(item.Get("type").String()) {
					outputs = append(outputs, item.Get("output").String())
				}
			}
			require.Equal(t, tc.outputs, outputs)
		})
	}
}

func TestVMKernelIngressRejectsMalformedJSON(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, ExecutionMode: AccountExecutionModeVMKernel}
	require.ErrorContains(t, validateVMKernelIngress(account, []byte(`{"input":`)), "invalid JSON request")
}

const vmContinuationToolResults = `[{"type":"custom_tool_call_output","call_id":"call_fixture","output":"done"},
	{"type":"function_call_output","call_id":"call_function","output":"lookup result"},
	{"type":"function_call_output","call_id":"fco_missing","output":"orphan continuation result"}]`

func assertVMKernelContinuationToolResults(t *testing.T, body []byte) {
	t.Helper()
	for _, pair := range []struct{ call, output, value string }{
		{"custom_tool_call", "custom_tool_call_output", "done"},
		{"function_call", "function_call_output", "lookup result"},
	} {
		call := gjson.GetBytes(body, fmt.Sprintf(`input.#(type==%q)`, pair.call))
		outputs := gjson.GetBytes(body, fmt.Sprintf(`input.#(type==%q)#`, pair.output)).Array()
		if len(outputs) != 1 {
			t.Errorf("expected one paired %s, got %d", pair.output, len(outputs))
			continue
		}
		if !call.Exists() || call.Get("call_id").String() == "" || outputs[0].Get("call_id").String() != call.Get("call_id").String() || outputs[0].Get("output").String() != pair.value {
			t.Errorf("continuation lost or altered the paired %s", pair.output)
		}
	}
}
