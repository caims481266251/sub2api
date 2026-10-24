package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestVMKernelCacheIdentitySurvivesTurnsAndReconnects(t *testing.T) {
	account := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{DockerRequestCacheEnabledKey: true}, ExecutionMode: AccountExecutionModeDockerKernel, Credentials: map[string]any{"chatgpt_account_id": "workspace", "chatgpt_user_id": "user"}}
	prepare := func(keyID int64, a *Account, body, header, ingress string) string {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		c.Set("api_key", &APIKey{ID: keyID})
		c.Request.Header.Set("session_id", header)
		prepared, _, _, err := (&OpenAIGatewayService{}).prepareVMKernelResponses(context.Background(), c, a, []byte(body), ingress, "")
		require.NoError(t, err)
		key := gjson.GetBytes(prepared, "prompt_cache_key").String()
		require.NotEmpty(t, key)
		return key
	}
	first := `{"model":"gpt-6-luna","prompt_cache_key":"caller-key","input":[{"role":"user","content":"hello"}]}`
	next := `{"model":"gpt-6-luna","prompt_cache_key":"caller-key","input":[{"role":"user","content":"hello"},{"role":"assistant","content":"ok"},{"role":"user","content":"next"}]}`
	key := prepare(10, account, first, "", "responses")
	require.Equal(t, key, prepare(10, account, next, "", "responses"))
	require.Equal(t, key, prepare(10, account, first, "rotating-irrelevant-header", "responses"), "explicit prompt key wins")
	require.NotEqual(t, "caller-key", key)
	require.NotEqual(t, key, prepare(11, account, first, "", "responses"))
	other := *account
	other.Credentials = map[string]any{"chatgpt_account_id": "different", "chatgpt_user_id": "user"}
	require.NotEqual(t, key, prepare(10, &other, first, "", "responses"))
	plain := `{"model":"gpt-6-luna","input":[{"role":"user","content":"hello"}]}`
	appended := `{"model":"gpt-6-luna","input":[{"role":"user","content":"hello"},{"role":"user","content":"next"}]}`
	require.Equal(t, prepare(10, account, plain, "stable-session", "responses"), prepare(10, account, appended, "stable-session", "responses"))
	require.Equal(t, prepare(10, account, plain, "", "responses"), prepare(10, account, appended, "", "responses"))
	chat := `{"model":"gpt-6-luna","messages":[{"role":"user","content":"hello"}]}`
	chatNext := `{"model":"gpt-6-luna","messages":[{"role":"user","content":"hello"},{"role":"user","content":"next"}]}`
	require.Equal(t, prepare(10, account, chat, "", "chat"), prepare(10, account, chatNext, "", "chat"))
}

func TestVMKernelContinuationKeepsExplicitCacheIdentity(t *testing.T) {
	h := &vmKernelWSHistory{}
	first, err := h.prepare([]byte(`{"type":"response.create","model":"gpt-6-luna","prompt_cache_key":"caller-key","input":"hello"}`))
	require.NoError(t, err)
	require.NoError(t, h.complete(first, []byte(`{"id":"resp_one","output":[]}`)))
	next, err := h.prepare([]byte(`{"type":"response.create","previous_response_id":"resp_one","input":"next"}`))
	require.NoError(t, err)
	require.Equal(t, "caller-key", gjson.GetBytes(next, "prompt_cache_key").String())
	explicit, err := h.prepare([]byte(`{"type":"response.create","previous_response_id":"resp_one","prompt_cache_key":"new-key","input":"next"}`))
	require.NoError(t, err)
	require.Equal(t, "new-key", gjson.GetBytes(explicit, "prompt_cache_key").String())
}

func TestVMKernelCacheMetadataDoesNotRotatePerRequest(t *testing.T) {
	a := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ExecutionMode: AccountExecutionModeDockerKernel, Extra: map[string]any{DockerRequestCacheEnabledKey: true}, Credentials: map[string]any{"chatgpt_account_id": "upstream"}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/", nil)
	var last string
	for i := 0; i < 3; i++ {
		m := map[string]any{}
		applyVMKernelPromptCacheKey(c, a, m, "stable")
		applyCodexAccountIdentityClientMetadataMap(m, a, 0)
		raw, _ := json.Marshal(m)
		if i > 0 {
			require.Equal(t, last, string(raw))
		}
		last = string(raw)
	}
}

func TestVMKernelHeaderOnlySessionSurvivesContinuation(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/", nil)
	c.Request.Header.Set("session-id", "header-only")
	raw := []byte(`{"model":"gpt-6-luna","input":[{"role":"user","content":"hello"}]}`)
	replay := vmKernelReplayCacheBody(c, &Account{Platform: PlatformOpenAI, ExecutionMode: AccountExecutionModeDockerKernel, Extra: map[string]any{DockerRequestCacheEnabledKey: true}}, raw, "gpt-6-luna")
	h := &vmKernelWSHistory{}
	require.NoError(t, h.complete(replay, []byte(`{"id":"resp_header","output":[]}`)))
	next, err := h.prepare([]byte(`{"type":"response.create","previous_response_id":"resp_header","input":"next"}`))
	require.NoError(t, err)
	require.Equal(t, "header-only", gjson.GetBytes(next, "prompt_cache_key").String())
}

func TestDockerRequestCacheDefaultsAndValidation(t *testing.T) {
	for _, raw := range []any{nil, false, "true", 1, map[string]any{"enabled": true}, true} {
		a := &Account{Platform: PlatformOpenAI, ExecutionMode: AccountExecutionModeDockerKernel, Extra: map[string]any{DockerRequestCacheEnabledKey: raw}}
		enabled, valid := raw.(bool)
		require.Equal(t, !valid || enabled, a.DockerRequestCacheEnabled())
		a.ExecutionMode = AccountExecutionModeNormal
		require.False(t, a.DockerRequestCacheEnabled())
		err := ValidateDockerRequestCacheExtra(a.Extra)
		if raw == nil || valid {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
	}
	require.False(t, (*Account)(nil).DockerRequestCacheEnabled())
	require.NoError(t, ValidateDockerRequestCacheExtra(nil))
}

func TestDockerRequestCacheTogglePreservesConversation(t *testing.T) {
	a := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ExecutionMode: AccountExecutionModeDockerKernel, Credentials: map[string]any{"access_token": "test", "chatgpt_account_id": "workspace"}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Request.Header.Set("session-id", "caller-session")
	c.Set("api_key", &APIKey{ID: 10})
	body := []byte(`{"model":"gpt-6-luna","prompt_cache_key":"caller-key","prompt_cache_retention":"24h","input":[{"role":"user","content":"remember this"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{}}}]}`)
	h := &vmKernelWSHistory{}
	fingerprint := openAITurnRouteFingerprint(a)
	for i, enabled := range []bool{false, true, false} {
		a.Extra = map[string]any{DockerRequestCacheEnabledKey: enabled}
		require.Equal(t, fingerprint, openAITurnRouteFingerprint(a), "cache setting must not break an open WS binding")
		for _, ingress := range []string{"responses", "chat", "messages"} {
			input := body
			if ingress == "chat" {
				input = []byte(`{"model":"gpt-6-luna","messages":[{"role":"user","content":"remember this"}],"prompt_cache_key":"caller-key"}`)
			}
			if ingress == "messages" {
				input = []byte(`{"model":"gpt-6-luna","max_tokens":32,"messages":[{"role":"user","content":"remember this"}]}`)
			}
			prepared, _, _, err := (&OpenAIGatewayService{}).prepareVMKernelResponses(context.Background(), c, a, input, ingress, "")
			require.NoError(t, err)
			require.Equal(t, enabled, gjson.GetBytes(prepared, "prompt_cache_key").Exists(), ingress)
			require.Contains(t, string(prepared), "remember this")
		}
		replay := vmKernelReplayCacheBody(c, a, body, "gpt-6-luna")
		require.Equal(t, enabled, gjson.GetBytes(replay, "prompt_cache_key").Exists())
		responseID := fmt.Sprintf("resp_%d", i)
		require.NoError(t, h.complete(replay, []byte(fmt.Sprintf(`{"id":%q,"output":[{"type":"function_call","call_id":"call_lookup","name":"lookup","arguments":"{}"}]}`, responseID))))
		next, err := h.prepare([]byte(fmt.Sprintf(`{"type":"response.create","model":"gpt-6-luna","previous_response_id":%q,"input":[{"type":"function_call_output","call_id":"call_lookup","output":"found"}]}`, responseID)))
		require.NoError(t, err)
		require.Contains(t, string(next), "remember this")
		require.Contains(t, string(next), "function_call_output")
		require.Contains(t, string(next), "lookup")
		require.False(t, gjson.GetBytes(next, "previous_response_id").Exists())
		body = next
	}
}

func TestVMKernelRequestCacheDispatchToggle(t *testing.T) {
	envelopes := make(chan []byte, 1)
	runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		envelopes <- raw
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range vmForwardEvents("responses") {
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
	})
	gateway := &OpenAIGatewayService{vmKernel: runtime}
	initialCredential := vmCredentialRevision(account)
	initialRoute := vmAccountRouteRevision(account)
	for _, enabled := range []bool{false, true, false} {
		account.Extra = map[string]any{DockerRequestCacheEnabledKey: enabled}
		for _, stream := range []bool{false, true} {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			c.Request.Header.Set("session-id", "stable-header")
			body := []byte(fmt.Sprintf(`{"model":"gpt-6-luna","stream":%t,"input":"context must survive","prompt_cache_key":"caller-cache"}`, stream))
			result, err := gateway.forwardVMKernelResponses(context.Background(), c, account, body, "responses", "")
			require.NoError(t, err)
			require.NotNil(t, result)
			require.True(t, result.VMKernelUsageAuthoritative)
			env := <-envelopes
			key := gjson.GetBytes(env, "body.prompt_cache_key").String()
			require.Equal(t, enabled, key != "")
			require.Equal(t, key, gjson.GetBytes(env, "session.session_id").String())
			require.Equal(t, key, gjson.GetBytes(env, "headers.session-id").String())
			require.Contains(t, gjson.GetBytes(env, "body.input").Raw, "context must survive")
		}
		require.Equal(t, initialCredential, vmCredentialRevision(account))
		require.Equal(t, initialRoute, vmAccountRouteRevision(account))
		require.True(t, VMKernelRuntimeReady(account))
	}
}
