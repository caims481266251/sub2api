package service

import (
	"fmt"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/sjson"
)

const DockerRequestCacheEnabledKey = "docker_request_cache_enabled"

// Default on for existing and new accounts; an explicit false keeps caching disabled.
func (a *Account) DockerRequestCacheEnabled() bool {
	if a == nil || !a.IsVMKernel() || a.Platform != PlatformOpenAI {
		return false
	}
	enabled, configured := a.Extra[DockerRequestCacheEnabledKey].(bool)
	return !configured || enabled
}

func ValidateDockerRequestCacheExtra(extra map[string]any) error {
	if value, exists := extra[DockerRequestCacheEnabledKey]; exists && value != nil {
		if _, ok := value.(bool); !ok {
			return infraerrors.BadRequest("INVALID_DOCKER_REQUEST_CACHE", "Docker request cache setting must be a boolean")
		}
	}
	return nil
}

// Scheduling and Docker forwarding resolve the same raw conversation seed.
// The inbound model is already part of the common content fallback.
func vmKernelPromptCacheSeed(c *gin.Context, body []byte, _ string) string {
	return openAIRequestSessionSeed(c, body)
}

func applyVMKernelPromptCacheKey(c *gin.Context, account *Account, request map[string]any, seed string) {
	if seed == "" {
		return
	}
	// Resolve after the ordinary metadata rewrite. Optional metadata must not
	// change the derivation kind. Local account IDs also isolate containers that
	// import the same upstream credential; API keys retain tenant isolation.
	session := deriveStableUUIDv4(fmt.Sprintf("sub2api:docker-session:v1:key:%d:account:%d:credential:%s:seed:%s",
		getAPIKeyIDFromContext(c), account.ID, codexAccountIdentityNamespace(codexAccountIdentitySource(c, account)), seed))
	metadata, _ := request["client_metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["session_id"] = session
	if _, exists := metadata["session-id"]; exists {
		metadata["session-id"] = session
	}
	if raw, ok := metadata[openAIWSTurnMetadataHeader].(string); ok {
		metadata[openAIWSTurnMetadataHeader] = vmKernelScopeTurnMetadataSession(raw, session)
	}
	request["client_metadata"] = metadata
	delete(request, "conversation_id")
	delete(request, "session_id")
	// Inbound affinity aliases are consumed here; the upstream Responses
	// schema does not accept a top-level thread_id.
	delete(request, "thread_id")
	if account.DockerRequestCacheEnabled() {
		request["prompt_cache_key"] = session
	} else {
		delete(request, "prompt_cache_key")
		delete(request, "prompt_cache_retention")
	}
}

// Save the unscoped seed with replay history so a continuation without repeated
// session headers inherits it and the account namespace is not applied twice.
func vmKernelReplayCacheBody(c *gin.Context, account *Account, body []byte, model string) []byte {
	// Internal history retains the raw seed even with caching disabled. Cache
	// hints are removed at dispatch, not from conversation identity/replay.
	if seed := vmKernelPromptCacheSeed(c, body, model); seed != "" {
		if next, err := sjson.SetBytes(body, "prompt_cache_key", seed); err == nil {
			return next
		}
	}
	return body
}
