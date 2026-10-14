package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIModelErrorAfterStreamStarted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	_, err := c.Writer.WriteString(": keepalive\n\n")
	require.NoError(t, err)
	failure := &service.UpstreamFailoverError{StatusCode: http.StatusBadRequest, ResponseBody: []byte(`{"error":{"type":"invalid_request_error","code":"model_not_found","message":"The requested model is not supported"}}`)}
	(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, failure, true)
	require.Equal(t, http.StatusOK, w.Code, "committed SSE keeps its HTTP status")
	require.Contains(t, w.Body.String(), `"code":"model_not_found"`)
	require.Contains(t, w.Body.String(), "The requested model is not supported")
}

func TestOpenAIResponses_BorrowIncrementalHistory(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, route := range []string{"original", "replacement", "normal_fallback"} {
			t.Run(fmt.Sprintf("%s/stream=%t", route, stream), func(t *testing.T) {
				h, repo, upstream, _ := newBorrowHTTPFallbackHandler(t)
				w := borrowFallbackRequest(t, h, "", `"Remember code 97531"`, stream)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				if route == "replacement" {
					repo.restart = true
					repo.route.ID = "new-route"
				}
				if route == "normal_fallback" {
					repo.unavailable = true
				}
				w = borrowFallbackRequest(t, h, "resp_borrow_1", `"What was the code?"`, stream)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				require.Len(t, upstream.bodies, 2)
				input := gjson.GetBytes(upstream.bodies[1], "input").Array()
				require.Len(t, input, 3, "actual upstream request must contain the completed conversation")
				require.Contains(t, input[0].Raw, "97531")
				require.Equal(t, "assistant", input[1].Get("role").String())
				require.Contains(t, input[2].Raw, "What was the code?")
				require.False(t, gjson.GetBytes(upstream.bodies[1], "previous_response_id").Exists())
				if route != "normal_fallback" {
					w = borrowFallbackRequest(t, h, "resp_borrow_2", `"Repeat the code"`, stream)
					require.Equal(t, http.StatusOK, w.Code, w.Body.String())
					require.Len(t, gjson.GetBytes(upstream.bodies[2], "input").Array(), 5)
					require.Contains(t, string(upstream.bodies[2]), "97531")
				}
				require.Zero(t, repo.leases)
			})
		}
	}
}

func TestOpenAIResponses_BorrowToolOutputDelta(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			h, repo, upstream, _ := newBorrowHTTPFallbackHandler(t)
			upstream.responseOutput = `[{"type":"function_call","call_id":"call_lookup","name":"lookup","arguments":"{}"}]`
			w := borrowFallbackRequest(t, h, "", `"lookup demo"`, stream)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			upstream.responseOutput = ""
			w = borrowFallbackRequest(t, h, "resp_borrow_1", `[{"type":"function_call_output","call_id":"call_lookup","output":"TOOL_OK"}]`, stream)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Len(t, upstream.bodies, 2)
			items := gjson.GetBytes(upstream.bodies[1], "input").Array()
			require.Len(t, items, 3)
			require.Equal(t, "lookup", items[1].Get("name").String())
			// The existing Codex adapter normalizes call_* to fc_* on the wire.
			require.Equal(t, "fc_lookup", items[1].Get("call_id").String())
			require.Equal(t, items[1].Get("call_id").String(), items[2].Get("call_id").String())
			require.Equal(t, "TOOL_OK", items[2].Get("output").String())
			require.Zero(t, repo.leases)
		})
	}
}

func TestOpenAIResponses_BorrowEncryptedReasoningContinuation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, route := range []string{"original", "replacement", "unavailable"} {
			t.Run(fmt.Sprintf("%s/stream=%t", route, stream), func(t *testing.T) {
				h, repo, upstream, _ := newBorrowHTTPFallbackHandler(t)
				h.cfg.Gateway.BorrowBusinessRecoveryTimeoutSeconds = 1
				upstream.responseOutput = `[{"type":"reasoning","id":"rs_candy","summary":[],"encrypted_content":"observed-private-state"},{"type":"function_call","call_id":"call_candy","name":"lookup","arguments":"{\"counts\":[7,9,8,7,6,4]}"}]`
				w := borrowFallbackRequest(t, h, "", `"Remember the candy counts and call lookup"`, stream)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				upstream.responseOutput = ""
				if route == "replacement" {
					repo.restart = true
					repo.route.ID = "replacement-route"
				}
				if route == "unavailable" {
					repo.unavailable = true
				}
				w = borrowFallbackRequest(t, h, "resp_borrow_1", `[{"type":"function_call_output","call_id":"call_candy","output":"29"}]`, stream)
				if route == "unavailable" {
					require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
					require.Equal(t, []int64{11}, upstream.accountIDs, "opaque state must never reach the healthy fallback account")
				} else {
					require.Equal(t, http.StatusOK, w.Code, w.Body.String())
					require.Equal(t, []int64{11, 11}, upstream.accountIDs)
					items := gjson.GetBytes(upstream.bodies[1], "input").Array()
					require.Len(t, items, 4)
					require.Equal(t, "observed-private-state", items[1].Get("encrypted_content").String())
					require.Equal(t, "fc_candy", items[2].Get("call_id").String())
					require.Equal(t, items[2].Get("call_id").String(), items[3].Get("call_id").String())
				}
				require.Zero(t, repo.leases)
			})
		}
	}
}

func TestOpenAIResponses_ModelErrorSurvivesBorrowSelection(t *testing.T) {
	for _, endpoint := range []string{"responses", "chat/completions", "messages"} {
		for _, stream := range []bool{false, true} {
			for _, available := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/available=%t/stream=%t", endpoint, available, stream), func(t *testing.T) {
					h, repo, upstream, _ := newBorrowHTTPFallbackHandler(t)
					h.cfg.Gateway.BorrowBusinessRecoveryTimeoutSeconds = 1
					repo.unavailable = true
					upstream.failureAccountID = 12
					upstream.failureStatus = http.StatusBadRequest
					upstream.failureBody = `{"error":{"type":"invalid_request_error","code":"model_not_found","param":"model","message":"The requested model is not supported"}}`
					if available {
						upstream.afterFailure = func() { repo.unavailable = false }
					}
					start := time.Now()
					w := borrowFallbackEndpointRequest(t, h, "", `"hello"`, stream, endpoint)
					if available {
						require.Equal(t, http.StatusOK, w.Code, w.Body.String())
						require.Equal(t, []int64{12, 11}, upstream.accountIDs)
					} else {
						require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
						if endpoint != "messages" {
							require.Equal(t, "model_not_found", gjson.Get(w.Body.String(), "error.code").String())
							require.Equal(t, "model", gjson.Get(w.Body.String(), "error.param").String())
						}
						require.Equal(t, "invalid_request_error", gjson.Get(w.Body.String(), "error.type").String())
						require.Contains(t, w.Body.String(), "model is not supported")
						require.Equal(t, []int64{12}, upstream.accountIDs)
					}
					require.Less(t, time.Since(start), 800*time.Millisecond, "model failure must not wait for missing route inventory")
					require.Zero(t, repo.leases)
				})
			}
		}
	}
}
