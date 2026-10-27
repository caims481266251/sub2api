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

	"github.com/Wei-Shaw/sub2api/internal/vmkernel"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const vmUnsupportedModelMessage = "The 'gpt-6.1-sol' model is not supported when using Codex with a ChatGPT account."

func TestVMKernelOpenAIProviderErrorHTTPAndSSEFields(t *testing.T) {
	for _, ingress := range []string{"responses", "chat", "messages"} {
		for _, committed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s_committed_%t", ingress, committed), func(t *testing.T) {
				var attempts atomic.Int32
				runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
					if attempts.Add(1) == 1 {
						w.Header().Set("Content-Type", "application/json")
						w.Header().Set("Retry-After", "17")
						w.WriteHeader(http.StatusBadRequest)
						json.NewEncoder(w).Encode(gin.H{"error": gin.H{"code": "upstream_status", "type": "api_error", "message": vmUnsupportedModelMessage, "param": "model", "debug": "private-token"}})
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					for _, event := range vmForwardEvents("responses") {
						fmt.Fprintf(w, "data: %s\n\n", event)
					}
				})
				t.Cleanup(runtime.kernel.Close)
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				if committed {
					c.Header("Content-Type", "text/event-stream")
					_, _ = c.Writer.WriteString(": keepalive\n\n")
				}
				svc := &OpenAIGatewayService{vmKernel: runtime}
				result, err := svc.forwardVMKernelResponses(context.Background(), c, account, []byte(`{"model":"gpt-6.1-sol","input":"hi","stream":true}`), ingress, "")
				require.Error(t, err)
				require.Nil(t, result, "provider rejection must not fabricate usage")
				require.Equal(t, int32(1), attempts.Load(), "error presentation must never replay")
				require.True(t, VMKernelRuntimeReady(account))
				require.Zero(t, runtime.slot(account.ID).active)
				require.Equal(t, StatusActive, account.Status)
				require.True(t, account.Schedulable)
				payload := w.Body.String()
				path := "error"
				if committed {
					require.Equal(t, http.StatusOK, w.Code)
					require.Equal(t, 1, strings.Count(payload, "data: "))
					payload = strings.TrimSpace(strings.SplitN(payload, "data: ", 2)[1])
					if ingress == "responses" {
						path = "response.error"
						require.Equal(t, "response.failed", gjson.Get(payload, "type").String())
						require.Positive(t, gjson.Get(payload, "response.created_at").Int())
						require.True(t, gjson.Get(payload, "sequence_number").Exists())
					}
				} else {
					require.Equal(t, http.StatusBadRequest, w.Code)
					require.Equal(t, "17", w.Header().Get("Retry-After"))
				}
				require.Equal(t, "upstream_status", gjson.Get(payload, path+".code").String())
				require.Equal(t, "api_error", gjson.Get(payload, path+".type").String())
				require.Equal(t, "model", gjson.Get(payload, path+".param").String())
				require.Equal(t, vmUnsupportedModelMessage, gjson.Get(payload, path+".message").String())
				require.NotContains(t, payload, "private-token")
				// A subsequent successful request on the same runtime must still
				// complete, release its slot and retain authoritative usage.
				c, _ = gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				result, err = svc.forwardVMKernelResponses(context.Background(), c, account, []byte(`{"model":"gpt-5.6-terra","input":"hi","stream":false}`), "responses", "")
				require.NoError(t, err)
				require.True(t, result.VMKernelUsageAuthoritative)
				require.Equal(t, "response.completed", result.UpstreamTerminalEvent)
				require.Equal(t, int32(2), attempts.Load())
				require.Zero(t, runtime.slot(account.ID).active)
			})
		}
	}
}

func TestVMKernelOpenAIProviderErrorOptionalFields(t *testing.T) {
	for _, source := range []string{
		`{"type":"error","error":{"message":"Invalid request","code":null,"param":null}}`,
		`{"type":"error","error":{"message":"Invalid request"}}`,
		`{"type":"error","message":"Invalid request","code":"invalid_request","param":"input"}`,
	} {
		for _, committed := range []bool{false, true} {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			if committed {
				_, _ = c.Writer.WriteString(": keepalive\n\n")
			}
			reportVMKernelProviderError(c, &Account{Platform: PlatformOpenAI}, "responses", &vmkernel.Result{ErrorEvent: []byte(source)})
			actual := gjson.Get(w.Body.String(), "error")
			if committed {
				actual = gjson.Get(strings.SplitN(w.Body.String(), "data: ", 2)[1], "response.error")
			}
			expected := gjson.Get(source, "error")
			if !expected.IsObject() {
				expected = gjson.Parse(source)
			}
			for _, key := range []string{"code", "param"} {
				require.Equal(t, expected.Get(key).Raw, actual.Get(key).Raw)
			}
			require.False(t, actual.Get("type").Exists(), "missing error type must not become upstream_error or the SSE event type")
		}
	}
}

func TestVMKernelAccountProbePreservesUpstreamRejection(t *testing.T) {
	runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(gin.H{"error": gin.H{"code": "upstream_status", "type": "api_error", "message": vmUnsupportedModelMessage}})
	})
	t.Cleanup(runtime.kernel.Close)
	previous, loaded := vmKernelServices.Swap(PlatformOpenAI, runtime)
	t.Cleanup(func() {
		if loaded {
			vmKernelServices.Store(PlatformOpenAI, previous)
		} else {
			vmKernelServices.Delete(PlatformOpenAI)
		}
	})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)
	err := (&AccountTestService{}).testVMKernelConnection(c, account, "gpt-6.1-sol", "hi")
	require.EqualError(t, err, vmUnsupportedModelMessage)
	require.Contains(t, w.Body.String(), vmUnsupportedModelMessage)
	require.NotContains(t, w.Body.String(), "VM kernel test did not complete")
	require.Equal(t, 1, strings.Count(w.Body.String(), `"type":"error"`))
	require.True(t, VMKernelRuntimeReady(account))
}
