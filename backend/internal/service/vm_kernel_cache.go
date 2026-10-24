package service

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Match vm2api's contract: a stable caller session (or first-user seed) must
// reach both the prompt cache key and the kernel session, across HTTP requests
// and WS reconnects. Request IDs and connection IDs are never cache identities.
func vmKernelPromptCacheSeed(c *gin.Context, body []byte, model string) string {
	if key := strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String()); key != "" {
		return key
	}
	if key := explicitOpenAIHeaderSessionID(c); key != "" {
		return key
	}
	for _, path := range []string{"client_metadata.session_id", "conversation_id", "session_id"} {
		if key := strings.TrimSpace(gjson.GetBytes(body, path).String()); key != "" {
			return key
		}
	}
	if gjson.GetBytes(body, "messages").IsArray() {
		var req apicompat.ChatCompletionsRequest
		if json.Unmarshal(body, &req) == nil {
			return deriveCompatPromptCacheKey(&req, model)
		}
	}
	input := gjson.GetBytes(body, "input")
	seed := ""
	if input.Type == gjson.String {
		seed = input.String()
	} else {
		for _, item := range input.Array() {
			if item.Get("role").String() == "user" {
				content := item.Get("content")
				if content.Type == gjson.String {
					seed = content.String()
				} else {
					seed = normalizeCompatSeedJSON(json.RawMessage(content.Raw))
				}
				if strings.TrimSpace(seed) != "" {
					break
				}
			}
		}
	}
	if seed == "" {
		seed = gjson.GetBytes(body, "instructions").String()
	}
	if seed == "" {
		return ""
	}
	return "container_" + hashSensitiveValueForLog(seed)
}

func applyVMKernelPromptCacheKey(c *gin.Context, account *Account, request map[string]any, seed string) {
	if seed == "" {
		return
	}
	// Use the ordinary account/API-key namespace, exactly once. Older imported
	// accounts without upstream identity still need tenant and local-row isolation.
	if codexAccountIdentityNamespace(account) == "" {
		seed = generateSessionUUID(isolateOpenAISessionID(getAPIKeyIDFromContext(c), fmt.Sprintf("container:%d:%s", account.ID, seed)))
	}
	request["prompt_cache_key"] = seed
}

// Save the unscoped seed with replay history so a continuation without repeated
// session headers inherits it and the account namespace is not applied twice.
func vmKernelReplayCacheBody(c *gin.Context, body []byte, model string) []byte {
	if seed := vmKernelPromptCacheSeed(c, body, model); seed != "" {
		if next, err := sjson.SetBytes(body, "prompt_cache_key", seed); err == nil {
			return next
		}
	}
	return body
}
