package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const vmToolsRequest = `{"model":"gpt-6-luna","input":[{"role":"user","content":"Use a tool","namespace":"remove"}],"tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{}}},{"type":"custom","name":"patch","format":{"type":"text"}}]},{"type":"custom","name":"plain","format":{"type":"text"}},{"type":"tool_search","execution":"client"}],"tool_choice":"auto"}`

func vmToolContext(path string) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, path, nil)
	return c, w
}

func TestVMKernelToolsPreparationMatchesOrdinaryNamespacePolicy(t *testing.T) {
	for _, flatten := range []bool{false, true} {
		for _, ingress := range []string{"responses", "chat"} {
			t.Run(fmt.Sprintf("%s_flatten_%v", ingress, flatten), func(t *testing.T) {
				a := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, ExecutionMode: AccountExecutionModeVMKernel, Extra: map[string]any{"openai_responses_flatten_namespaces": flatten}}
				c, _ := vmToolContext("/v1/responses")
				want, err := normalizeOpenAIResponsesNamespaces(c, a, []byte(vmToolsRequest), OpenAIUpstreamTransportHTTPSSE, false, false)
				require.NoError(t, err)
				got, _, _, err := (&OpenAIGatewayService{}).prepareVMKernelResponses(context.Background(), c, a, []byte(vmToolsRequest), ingress, "")
				require.NoError(t, err)
				require.JSONEq(t, gjson.GetBytes(want, "tools").Raw, gjson.GetBytes(got, "tools").Raw)
				require.False(t, gjson.GetBytes(got, "input.0.namespace").Exists())
				require.Equal(t, "auto", gjson.GetBytes(got, "tool_choice").String())
				numberBody, err := sjson.SetRawBytes([]byte(vmToolsRequest), "sequence", []byte("9007199254740993"))
				require.NoError(t, err)
				got, _, _, err = (&OpenAIGatewayService{}).prepareVMKernelResponses(context.Background(), c, a, numberBody, ingress, "")
				require.NoError(t, err)
				require.Equal(t, "9007199254740993", gjson.GetBytes(got, "sequence").Raw)
				// A later attempt using the same Gin context must clear prior mappings.
				a.Extra["openai_responses_flatten_namespaces"] = false
				_, _, _, err = (&OpenAIGatewayService{}).prepareVMKernelResponses(context.Background(), c, a, []byte(vmToolsRequest), ingress, "")
				require.NoError(t, err)
				require.Empty(t, openAIResponsesNamespaceNames(c))
			})
		}
	}
}

func TestVMKernelToolsLiteNormalization(t *testing.T) {
	a := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, ExecutionMode: AccountExecutionModeVMKernel}
	c, _ := vmToolContext("/v1/responses")
	c.Request.Header.Set(responsesLiteHeader, "true")
	got, _, _, err := (&OpenAIGatewayService{}).prepareVMKernelResponses(context.Background(), c, a, []byte(vmToolsRequest), "responses", "")
	require.NoError(t, err)
	require.Equal(t, "all_turns", gjson.GetBytes(got, "reasoning.context").String())
	require.False(t, gjson.GetBytes(got, "parallel_tool_calls").Bool())
	require.False(t, gjson.GetBytes(got, `tools.#(type=="namespace")`).Exists())
	require.Equal(t, "functions", gjson.GetBytes(got, `input.#(type=="additional_tools").tools.0.name`).String())
}

func vmToolEvents(name, namespace, responseID string) []string {
	item := map[string]any{"id": "ct_fixture", "type": "custom_tool_call", "call_id": "call_fixture", "name": name, "input": "apply text", "opaque": map[string]any{"sequence": json.Number("9007199254740993")}}
	if namespace != "" {
		item["namespace"] = namespace
	}
	raw, _ := json.Marshal(item)
	functionName := "lookup"
	if namespace == "" {
		functionName = "functions__lookup"
	}
	function := map[string]any{"id": "fc_fixture", "type": "function_call", "call_id": "call_function", "name": functionName, "arguments": "{}"}
	if namespace != "" {
		function["namespace"] = namespace
	}
	functionRaw, _ := json.Marshal(function)
	searchRaw, _ := json.Marshal(map[string]any{"id": "search_fixture", "type": "tool_search_call", "status": "completed", "opaque": json.Number("9007199254740993")})
	output := string(raw) + "," + string(functionRaw) + "," + string(searchRaw)
	return []string{
		`{"type":"response.output_item.added","output_index":0,"item":` + string(raw) + `}`,
		`{"type":"response.custom_tool_call_input.delta","item_id":"ct_fixture","output_index":0,"delta":"apply text"}`,
		`{"type":"response.output_item.done","output_index":0,"item":` + string(raw) + `}`,
		`{"type":"response.output_item.done","output_index":1,"item":` + string(functionRaw) + `}`,
		`{"type":"response.output_item.done","output_index":2,"item":` + string(searchRaw) + `}`,
		`{"type":"response.completed","response":{"id":"` + responseID + `","object":"response","created_at":123,"status":"completed","model":"gpt-6-luna","output":[` + output + `],"usage":{"input_tokens":7,"output_tokens":3},"provider_extension":{"value":true}}}`,
	}
}

func TestVMKernelToolsHTTPUDSRoundTrip(t *testing.T) {
	for _, flatten := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("flatten_%v_stream_%v", flatten, stream), func(t *testing.T) {
				var calls atomic.Int32
				runtime, a := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					var env struct {
						Body json.RawMessage `json:"body"`
					}
					if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
						t.Error(err)
						return
					}
					name, ns := "patch", "functions"
					if flatten {
						name, ns = "functions__patch", ""
					}
					if !strings.Contains(string(env.Body), name) {
						t.Error("tool declaration missing")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					for _, event := range vmToolEvents(name, ns, "resp_tool") {
						_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
					}
				}, func(a *Account) { a.Extra = map[string]any{"openai_responses_flatten_namespaces": flatten} })
				c, w := vmToolContext("/v1/responses")
				body, err := sjson.SetBytes([]byte(vmToolsRequest), "stream", stream)
				require.NoError(t, err)
				result, err := (&OpenAIGatewayService{cfg: &config.Config{}, vmKernel: runtime}).forwardVMKernelResponses(context.Background(), c, a, body, "responses", "")
				require.NoError(t, err)
				require.True(t, result.VMKernelUsageAuthoritative)
				require.EqualValues(t, 1, calls.Load())
				require.Equal(t, http.StatusOK, w.Code)
				require.Contains(t, w.Body.String(), `"namespace":"functions"`)
				require.Contains(t, w.Body.String(), `"name":"patch"`)
				require.Contains(t, w.Body.String(), `"type":"custom_tool_call"`)
				require.NotContains(t, w.Body.String(), "functions__patch")
				require.Contains(t, w.Body.String(), "9007199254740993")
				if !stream {
					require.True(t, gjson.Get(w.Body.String(), "provider_extension.value").Bool())
				}
			})
		}
	}
}

func TestVMKernelToolsWSUDSContinuation(t *testing.T) {
	for _, flatten := range []bool{false, true} {
		t.Run(fmt.Sprintf("flatten_%v", flatten), func(t *testing.T) {
			var attempts atomic.Int32
			runtime, a := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
				n := attempts.Add(1)
				var env struct {
					Body json.RawMessage `json:"body"`
				}
				if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
					t.Error(err)
					return
				}
				name, ns := "patch", "functions"
				if flatten {
					name, ns = "functions__patch", ""
				}
				if n == 2 {
					call := gjson.GetBytes(env.Body, `input.#(type=="custom_tool_call")`)
					if call.Get("name").String() != name || call.Get("namespace").String() != ns {
						t.Error("replay tool identity changed")
					}
					for _, item := range gjson.GetBytes(env.Body, "input").Array() {
						if item.Get("type").String() == "function_call" {
							wantName := "lookup"
							if flatten {
								wantName = "functions__lookup"
							}
							if item.Get("name").String() != wantName || item.Get("namespace").String() != ns {
								t.Error("function replay identity changed")
							}
						}
					}
					if !gjson.GetBytes(env.Body, `input.#(type=="custom_tool_call_output")`).Exists() {
						t.Error("tool result lost")
					}
				}
				w.Header().Set("Content-Type", "text/event-stream")
				for _, event := range vmToolEvents(name, ns, fmt.Sprintf("resp_tool_%d", n)) {
					if gjson.Get(event, "type").String() == "response.completed" {
						event, _ = sjson.SetRaw(event, "response.output", "[]")
					}
					_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
				}
			}, func(a *Account) { a.Extra = map[string]any{"openai_responses_flatten_namespaces": flatten} })
			svc := &OpenAIGatewayService{cfg: &config.Config{}, vmKernel: runtime}
			done := make(chan error, 1)
			first, err := sjson.SetBytes([]byte(vmToolsRequest), "type", "response.create")
			require.NoError(t, err)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					done <- err
					return
				}
				defer func() { _ = conn.CloseNow() }()
				c, _ := gin.CreateTestContext(w)
				c.Request = r
				done <- svc.proxyVMKernelWebSocket(r.Context(), c, conn, a, first, nil)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			require.NoError(t, err)
			defer func() { _ = client.CloseNow() }()
			for turn := 1; turn <= 2; turn++ {
				for {
					_, data, err := client.Read(ctx)
					require.NoError(t, err)
					if gjson.GetBytes(data, "type").String() == "response.completed" {
						require.Equal(t, "functions", gjson.GetBytes(data, "response.output.0.namespace").String())
						require.Equal(t, "patch", gjson.GetBytes(data, "response.output.0.name").String())
						require.Equal(t, "lookup", gjson.GetBytes(data, "response.output.1.name").String())
						require.Equal(t, "functions", gjson.GetBytes(data, "response.output.1.namespace").String())
						require.Equal(t, "tool_search_call", gjson.GetBytes(data, "response.output.2.type").String())
						break
					}
				}
				if turn == 1 {
					next, err := sjson.SetBytes(first, "previous_response_id", "resp_tool_1")
					require.NoError(t, err)
					next, err = sjson.SetRawBytes(next, "input", []byte(`[{"type":"custom_tool_call_output","call_id":"call_fixture","output":"done"}]`))
					require.NoError(t, err)
					require.NoError(t, client.Write(ctx, coderws.MessageText, next))
				}
			}
			_ = client.CloseNow()
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			require.EqualValues(t, 2, attempts.Load())
		})
	}
}
