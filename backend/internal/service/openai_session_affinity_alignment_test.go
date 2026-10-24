package service

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func affinityContext(keyID int64) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Set("api_key", &APIKey{ID: keyID, Group: &Group{Platform: PlatformOpenAI}})
	return c
}

func affinityBody(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(fields)
	require.NoError(t, err)
	return b
}

func TestOpenAISessionAffinityCanonicalSignals(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 31, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ExecutionMode: AccountExecutionModeDockerKernel}
	for _, signal := range []string{"session-id", "thread-id", "prompt_cache_key", "client_metadata.session_id", "conversation_id", "session_id", "thread_id"} {
		t.Run(signal, func(t *testing.T) {
			c := affinityContext(11)
			fields := map[string]any{"model": "gpt-5", "input": "first"}
			switch signal {
			case "session-id", "thread-id":
				c.Request.Header.Set(signal, "conversation-A")
				fields["prompt_cache_key"] = "conflicting-cache-key"
			case "client_metadata.session_id":
				fields["client_metadata"] = map[string]any{"session_id": "conversation-A"}
			default:
				fields[signal] = "conversation-A"
			}
			body := affinityBody(t, fields)
			want, _ := deriveOpenAIRequestSessionHashes(c, "conversation-A")
			require.Equal(t, want, svc.GenerateSessionHash(c, body))
			require.Equal(t, "conversation-A", vmKernelPromptCacheSeed(c, body, "mapped-model"))
			prepared, _, _, err := svc.prepareVMKernelResponses(context.Background(), c, account, body, "responses", "")
			require.NoError(t, err)
			session := vmKernelPreparedSession(prepared)
			require.NotEmpty(t, session)
			require.Equal(t, session, gjson.GetBytes(prepared, "prompt_cache_key").String())
			// Explicit conversation identity survives changing tools, model, and text.
			fields["model"], fields["input"], fields["instructions"] = "gpt-6-luna", "next", "changed instructions"
			if signal == "session-id" || signal == "thread-id" {
				fields["prompt_cache_key"] = "rotated-cache-key"
			}
			fields["tools"] = []any{map[string]any{"type": "function", "name": "lookup"}}
			next := affinityBody(t, fields)
			require.Equal(t, want, svc.GenerateSessionHash(c, next))
			require.Equal(t, "conversation-A", vmKernelPromptCacheSeed(c, next, "other-mapped-model"))
			wrapped := affinityBody(t, map[string]any{"type": "response.create", "response": fields})
			require.Equal(t, want, svc.GenerateSessionHash(c, wrapped))
			require.Equal(t, "conversation-A", vmKernelPromptCacheSeed(c, wrapped, ""))
		})
	}
}

func TestOpenAISessionAffinityContentFallbackMatchesDocker(t *testing.T) {
	svc := &OpenAIGatewayService{}
	for _, ingress := range []string{"responses", "chat"} {
		t.Run(ingress, func(t *testing.T) {
			c := affinityContext(12)
			fields := map[string]any{"model": "gpt-5", "instructions": "stable"}
			key := "input"
			if ingress == "chat" {
				key = "messages"
			}
			fields[key] = []any{map[string]any{"role": "user", "content": "hello"}}
			body := affinityBody(t, fields)
			seed := deriveOpenAIContentSessionSeed(body)
			require.Equal(t, seed, vmKernelPromptCacheSeed(c, body, "mapped-model"))
			want, _ := deriveOpenAIRequestSessionHashes(c, seed)
			require.Equal(t, want, svc.GenerateSessionHash(c, body))
			fields[key] = append(fields[key].([]any), map[string]any{"role": "assistant", "content": "ok"}, map[string]any{"role": "user", "content": "next"})
			next := affinityBody(t, fields)
			require.Equal(t, want, svc.GenerateSessionHash(c, next))
			require.Equal(t, seed, vmKernelPromptCacheSeed(c, next, "different-mapping"))
			fields["instructions"] = "changed"
			changed := affinityBody(t, fields)
			require.NotEqual(t, want, svc.GenerateSessionHash(c, changed))
			require.NotEqual(t, seed, vmKernelPromptCacheSeed(c, changed, ""))
		})
	}
}

func TestOpenAISessionAffinityTenantIsolationAndLegacyFallback(t *testing.T) {
	cache := &stubGatewayCache{sessionBindings: map[string]int64{}}
	svc := &OpenAIGatewayService{cache: cache}
	c1, c2 := affinityContext(101), affinityContext(102)
	body := affinityBody(t, map[string]any{"prompt_cache_key": "shared"})
	h1, h2 := svc.GenerateSessionHash(c1, body), svc.GenerateSessionHash(c2, body)
	require.NotEqual(t, h1, h2)
	unscoped, old := deriveOpenAISessionHashes("shared")
	cache.sessionBindings["openai:"+unscoped] = 998
	cache.sessionBindings["openai:"+old] = 999
	require.NotEqual(t, old, openAILegacySessionHashFromContext(c1.Request.Context()))
	require.NoError(t, svc.BindStickySession(c1.Request.Context(), nil, h1, 31))
	id, err := svc.getStickySessionAccountID(c2.Request.Context(), nil, h2)
	require.Error(t, err)
	require.Zero(t, id, "another key cannot inherit either hash format")
	delete(cache.sessionBindings, "openai:"+h1)
	id, err = svc.getStickySessionAccountID(c1.Request.Context(), nil, h1)
	require.NoError(t, err)
	require.Equal(t, int64(31), id, "tenant-scoped legacy fallback remains supported")
	require.NotEqual(t, svc.GenerateExplicitSessionHash(c1, body), svc.GenerateExplicitSessionHash(c2, body))
	require.NotEqual(t, svc.GenerateSessionHashWithFallback(c1, nil, "fallback"), svc.GenerateSessionHashWithFallback(c2, nil, "fallback"))
	content := affinityBody(t, map[string]any{"model": "gpt-5", "input": "same content"})
	require.NotEqual(t, svc.GenerateSessionHash(c1, content), svc.GenerateSessionHash(c2, content))
	// Parent review affinity must resolve the same scoped namespace as its parent.
	c1.Request.Header.Set(openAISubagentHeader, "guardian")
	c1.Request.Header.Set(codexParentThreadIDHeader, "shared")
	ctx := WithOpenAIGuardianParentAffinity(context.Background(), c1, nil, codexAutoReviewModel)
	require.Equal(t, int64(31), svc.resolveOpenAIGuardianParentAccountID(ctx, nil))
	c2.Request.Header.Set(openAISubagentHeader, "guardian")
	c2.Request.Header.Set(codexParentThreadIDHeader, "shared")
	ctx2 := WithOpenAIGuardianParentAffinity(context.Background(), c2, nil, codexAutoReviewModel)
	require.Zero(t, svc.resolveOpenAIGuardianParentAccountID(ctx2, nil))
}

func TestOpenAISessionAffinityGrokUnchanged(t *testing.T) {
	svc := &OpenAIGatewayService{}
	c := affinityContext(101)
	c.Set("api_key", &APIKey{ID: 101, Group: &Group{Platform: PlatformGrok}})
	c.Request.Header.Set("thread-id", "openai-only")
	body := affinityBody(t, map[string]any{"model": "grok-test", "session_id": "openai-body-alias", "input": "hello"})
	require.Empty(t, svc.ExtractSessionID(c, body))
	seed := grokStickyAffinitySeed(deriveOpenAIContentSessionSeed(body), body)
	want, _ := deriveOpenAISessionHashes(seed)
	require.Equal(t, want, svc.GenerateSessionHash(c, body))
}

func TestOpenAISessionAffinitySpilloverSurvivesAdmission(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		for _, acquired := range []bool{false, true} {
			name := "legacy"
			if advanced {
				name = "advanced"
			}
			if acquired {
				name += "/immediate"
			} else {
				name += "/wait"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				groupID := int64(7)
				cache := &stubGatewayCache{sessionBindings: map[string]int64{"openai:spill": 1}}
				accounts := []Account{
					{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 2, Priority: 1, GroupIDs: []int64{groupID}},
					{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 2, Priority: 0, GroupIDs: []int64{groupID}},
				}
				cfg := &config.Config{RunMode: config.RunModeStandard}
				cfg.Gateway.Scheduling.LoadBatchEnabled = true
				cfg.Gateway.Scheduling.StickySessionMaxWaiting = 1
				cfg.Gateway.Scheduling.FallbackWaitTimeout = time.Second
				cfg.Gateway.OpenAIWS.LBTopK = 1
				cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = true
				cc := stubConcurrencyCache{acquireResults: map[int64]bool{1: false, 2: acquired}, waitCounts: map[int64]int{1: 1}, loadMap: map[int64]*AccountLoadInfo{1: {AccountID: 1, LoadRate: 100}, 2: {AccountID: 2, LoadRate: 10}}}
				svc := &OpenAIGatewayService{accountRepo: stubOpenAIAccountRepo{accounts: accounts}, cache: cache, cfg: cfg, concurrencyService: NewConcurrencyService(cc)}
				var selection *AccountSelectionResult
				var err error
				if advanced {
					scheduler := newDefaultOpenAIAccountScheduler(svc, newOpenAIAccountRuntimeStats())
					selection, _, err = scheduler.Select(ctx, OpenAIAccountScheduleRequest{GroupID: &groupID, Platform: PlatformOpenAI, SessionHash: "spill", RequestedModel: "gpt-5", RequiredTransport: OpenAIUpstreamTransportAny})
				} else {
					selection, err = svc.SelectAccountWithLoadAwareness(ctx, &groupID, "spill", "gpt-5", nil)
				}
				require.NoError(t, err)
				require.NotNil(t, selection)
				require.Equal(t, int64(2), selection.Account.ID)
				require.Equal(t, acquired, selection.Acquired)
				require.True(t, selection.PreserveStickyBinding)
				if !acquired {
					require.NotNil(t, selection.WaitPlan)
				}
				require.NoError(t, svc.BindStickySessionAfterSelection(ctx, &groupID, "spill", selection))
				require.Equal(t, int64(1), cache.sessionBindings["openai:spill"])
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
				// Ordinary new placement/failover still binds; this flag adds no account lock.
				selection.PreserveStickyBinding = false
				require.NoError(t, svc.BindStickySessionAfterSelection(ctx, &groupID, "spill", selection))
				require.Equal(t, int64(2), cache.sessionBindings["openai:spill"])
			})
		}
	}
}
