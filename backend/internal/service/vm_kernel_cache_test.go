package service

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestVMKernelCacheIdentitySurvivesTurnsAndReconnects(t *testing.T) {
	account := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ExecutionMode: AccountExecutionModeDockerKernel, Credentials: map[string]any{"chatgpt_account_id": "workspace", "chatgpt_user_id": "user"}}
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
	a := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "upstream"}}
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
	replay := vmKernelReplayCacheBody(c, raw, "gpt-6-luna")
	h := &vmKernelWSHistory{}
	require.NoError(t, h.complete(replay, []byte(`{"id":"resp_header","output":[]}`)))
	next, err := h.prepare([]byte(`{"type":"response.create","previous_response_id":"resp_header","input":"next"}`))
	require.NoError(t, err)
	require.Equal(t, "header-only", gjson.GetBytes(next, "prompt_cache_key").String())
}
