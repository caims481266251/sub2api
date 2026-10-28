package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestVMKernelTurnHeadersRoundTripAndAccountGuard(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, foreign := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/foreign=%v", stream, foreign), func(t *testing.T) {
				runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
					var env struct {
						Headers map[string]string
						Body    struct {
							ClientMetadata map[string]any `json:"client_metadata"`
						}
					}
					require.NoError(t, json.NewDecoder(r.Body).Decode(&env))
					wantState := "caller-state"
					if foreign {
						wantState = ""
					}
					require.Equal(t, wantState, env.Headers[openAICodexTurnStateHeader])
					require.Nil(t, env.Body.ClientMetadata, "turn context travels in guarded headers only")
					require.Equal(t, "turn-from-payload", env.Headers[openAIWSTurnMetadataHeader])
					require.Equal(t, "beta-feature", env.Headers["x-codex-beta-features"])
					require.Empty(t, env.Headers["authorization"])
					require.Empty(t, env.Headers["cookie"])
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"type\":\"response_headers\",\"headers\":{\"x-codex-turn-state\":\"new-state\",\"authorization\":\"private\"}}\n\n")
					for _, e := range vmForwardEvents("responses") {
						fmt.Fprintf(w, "data: %s\n\n", e)
					}
				})
				svc := &OpenAIGatewayService{vmKernel: runtime}
				c, recorder := newTurnStateTestContext(t, 11, "header-roundtrip")
				c.Request.Header.Set(openAICodexTurnStateHeader, "caller-state")
				c.Request.Header.Set(openAIWSTurnMetadataHeader, "handshake-metadata")
				c.Request.Header.Set("x-codex-beta-features", "beta-feature")
				c.Request.Header.Set("Authorization", "secret")
				c.Request.Header.Set("Cookie", "secret")
				prior := *account
				if foreign {
					prior.ID++
				}
				svc.noteOpenAICodexTurnStateProvenance(c, &prior)
				payload, _ := json.Marshal(map[string]any{"model": "gpt-5", "input": "hello", "stream": stream, "client_metadata": map[string]string{"x-codex-turn-metadata": "turn-from-payload", "x-codex-turn-state": "caller-state"}})
				result, err := svc.forwardVMKernelResponses(context.Background(), c, account, payload, "responses", "")
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, "new-state", recorder.Result().Header.Get(openAICodexTurnStateHeader), "must commit before the first SSE write")
				require.Empty(t, recorder.Result().Header.Get("Authorization"))
				out := http.Header{}
				out.Set(openAICodexTurnStateHeader, "new-state")
				svc.guardOpenAICodexTurnStateEcho(c, account, out)
				require.Equal(t, "new-state", out.Get(openAICodexTurnStateHeader))
				other := *account
				other.ID++
				svc.guardOpenAICodexTurnStateEcho(c, &other, out)
				require.Empty(t, out.Get(openAICodexTurnStateHeader))
			})
		}
	}
}

func TestVMKernelTurnStateBodyAliasesAndForeignAccount(t *testing.T) {
	svc := &OpenAIGatewayService{}
	a := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ExecutionMode: AccountExecutionModeDockerKernel, Credentials: map[string]any{"chatgpt_account_id": "shared"}}
	prepare := func(account *Account, header, seed string) (*gin.Context, []byte) {
		c, _ := newTurnStateTestContext(t, 11, "")
		fields := map[string]any{"model": "gpt-5", "input": "hello"}
		if header == "" {
			fields["prompt_cache_key"] = seed
		} else {
			c.Request.Header.Set(header, seed)
		}
		raw, _ := json.Marshal(fields)
		body, _, _, err := svc.prepareVMKernelResponses(context.Background(), c, account, raw, "responses", "")
		require.NoError(t, err)
		return c, body
	}
	for _, header := range []string{"", "X-Session-Id", "X-Conversation-ID", "thread-id"} {
		c, body := prepare(a, header, "session")
		svc.noteVMKernelTurnState(c, a, body, "opaque-state")
		for _, changed := range []string{"same", "account", "session"} {
			other := *a
			seed := "session"
			if changed == "account" {
				other.ID++
			}
			if changed == "session" {
				seed = "different"
			}
			ctx, prepared := prepare(&other, header, seed)
			h := http.Header{}
			h.Set(openAICodexTurnStateHeader, "opaque-state")
			svc.guardVMKernelTurnState(ctx, &other, prepared, h)
			require.Equal(t, changed == "same", h.Get(openAICodexTurnStateHeader) != "", header+changed)
		}
	}
}

func TestVMKernelLateTurnMetadataDeliveredWithoutPrivateHeaders(t *testing.T) {
	for _, placement := range []string{"early", "late", "trailer"} {
		t.Run(placement, func(t *testing.T) {
			runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Trailer", "X-Codex-Turn-State")
				metadata, _ := json.Marshal(map[string]any{"type": "codex.response.metadata", "headers": map[string]string{"x-codex-turn-state": "delivered-state", "authorization": "private-secret"}})
				for i, e := range vmForwardEvents("responses") {
					if (placement == "early" && i == 0) || (placement == "late" && i == 2) {
						fmt.Fprintf(w, "data: %s\n\n", metadata)
					}
					fmt.Fprintf(w, "data: %s\n\n", e)
					w.(http.Flusher).Flush()
				}
				if placement == "trailer" {
					w.Header().Set("X-Codex-Turn-State", "delivered-state")
				}
			})
			svc := &OpenAIGatewayService{vmKernel: runtime}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			body, _ := json.Marshal(map[string]any{"model": "gpt-5", "input": "hello", "stream": true, "prompt_cache_key": "stable"})
			result, err := svc.forwardVMKernelResponses(context.Background(), c, account, body, "responses", "")
			require.NoError(t, err)
			require.True(t, result.VMKernelUsageAuthoritative)
			out := rec.Body.String()
			require.Equal(t, 1, strings.Count(out, "event: codex.response.metadata"))
			require.NotContains(t, out, "private-secret")
			require.Less(t, strings.Index(out, "event: codex.response.metadata"), strings.Index(out, "event: response.completed"))
			if placement == "early" {
				require.Equal(t, "delivered-state", rec.Result().Header.Get(openAICodexTurnStateHeader))
			}
			for _, line := range strings.Split(out, "\n") {
				if strings.HasPrefix(line, "data: ") && gjson.Get(line[6:], "type").String() == "codex.response.metadata" {
					require.Equal(t, "delivered-state", gjson.Get(line[6:], "headers.x-codex-turn-state").String())
				}
			}
		})
	}
}

func TestVMKernelTurnMetadataSessionMatchesEnvelope(t *testing.T) {
	runtime, account := vmForwardFixture(t, PlatformOpenAI, func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			Body    json.RawMessage
			Headers map[string]string
			Session map[string]string
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&env))
		session := env.Session["session_id"]
		require.NotEmpty(t, session)
		require.Equal(t, session, env.Headers["session-id"])
		require.False(t, gjson.GetBytes(env.Body, "client_metadata").Exists())
		metadata := env.Headers[openAIWSTurnMetadataHeader]
		require.Equal(t, session, gjson.Get(metadata, "session_id").String())
		require.Equal(t, "caller-extra", gjson.Get(metadata, "custom").String())
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range vmForwardEvents("responses") {
			fmt.Fprintf(w, "data: %s\n\n", e)
		}
	})
	svc := &OpenAIGatewayService{vmKernel: runtime}
	c, _ := newTurnStateTestContext(t, 11, "header-session")
	headerMetadata, _ := json.Marshal(map[string]string{"session_id": "header-session", "custom": "old-header"})
	c.Request.Header.Set(openAIWSTurnMetadataHeader, string(headerMetadata))
	bodyMetadata, _ := json.Marshal(map[string]string{"session_id": "body-session", "custom": "caller-extra"})
	body, _ := json.Marshal(map[string]any{"model": "gpt-5", "input": "hello", "prompt_cache_key": "cache-session", "client_metadata": map[string]string{"session_id": "body-session", "x-codex-turn-metadata": string(bodyMetadata)}})
	_, err := svc.forwardVMKernelResponses(context.Background(), c, account, body, "responses", "")
	require.NoError(t, err)
}
