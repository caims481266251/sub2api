package service

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func vmKernelPreparedSession(body []byte) string {
	return strings.TrimSpace(gjson.GetBytes(body, "client_metadata.session_id").String())
}

// Only the session field is synchronized. Turn/thread IDs have their own
// lifetime and unrelated metadata is preserved.
func vmKernelScopeTurnMetadataSession(raw, session string) string {
	var metadata map[string]any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&metadata) != nil || metadata == nil {
		return raw
	}
	changed := false
	for _, key := range []string{"session_id", "session-id"} {
		if _, exists := metadata[key]; exists {
			metadata[key] = session
			changed = true
		}
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(metadata)
	if err != nil {
		return raw
	}
	return string(out)
}

func vmKernelMetadataState(data []byte) string {
	return gjson.GetBytes(data, "headers."+openAICodexTurnStateHeader).String()
}

// Track the actual delivered blob, not just the last account selected for a
// session. This covers every supported header/body session alias and concurrent
// failover without retaining opaque provider state in the provenance table.
func vmKernelTurnStateOriginKey(c *gin.Context, state string) string {
	return fmt.Sprintf("docker-turn-state:%d:%x", getAPIKeyIDFromContext(c), sha256.Sum256([]byte(state)))
}

func (s *OpenAIGatewayService) noteVMKernelTurnState(c *gin.Context, account *Account, body []byte, state string) {
	session := vmKernelPreparedSession(body)
	if s == nil || account == nil || state == "" || session == "" {
		return
	}
	s.openaiCodexTurnStateOrigins.Store(vmKernelTurnStateOriginKey(c, state), openAICodexTurnStateOrigin{
		accountID: account.ID, sessionID: session, expiresAt: time.Now().Add(s.openAIWSSessionStickyTTL()),
	})
	s.sweepOpenAICodexTurnStateOrigins()
}

func (s *OpenAIGatewayService) guardVMKernelTurnState(c *gin.Context, account *Account, body []byte, headers http.Header) {
	state := headers.Get(openAICodexTurnStateHeader)
	if s == nil || account == nil || state == "" {
		return
	}
	key := vmKernelTurnStateOriginKey(c, state)
	if raw, ok := s.openaiCodexTurnStateOrigins.Load(key); ok {
		if origin, valid := raw.(openAICodexTurnStateOrigin); valid && time.Now().Before(origin.expiresAt) {
			if origin.accountID != account.ID || origin.sessionID != vmKernelPreparedSession(body) {
				headers.Del(openAICodexTurnStateHeader)
			}
			return
		}
		s.openaiCodexTurnStateOrigins.Delete(key)
	}
	// Preserve ordinary-account interoperability and unknown-state passthrough.
	s.guardOpenAICodexTurnStateEcho(c, account, headers)
}
