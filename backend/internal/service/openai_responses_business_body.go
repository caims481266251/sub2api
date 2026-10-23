package service

import "github.com/Wei-Shaw/sub2api/internal/config"

// normalizeOpenAIResponsesBusinessBody is shared by host and guest execution.
// Execution mode must not change legacy ingress, tool schema or reasoning rules.
func normalizeOpenAIResponsesBusinessBody(cfg *config.Config, account *Account, body []byte, upstreamModel string) ([]byte, error) {
	if normalized, changed, err := normalizeOpenAIResponsesLegacyIngressWithConfig(cfg, body); err != nil {
		return nil, err
	} else if changed {
		body = normalized
	}
	if normalized, changed, err := sanitizeOpenAIResponsesToolSchemasForPlatform(body, account.Platform); err != nil {
		return nil, err
	} else if changed {
		body = normalized
	}
	if account.IsOpenAIOAuthLike() {
		if normalized, changed, err := normalizeOpenAIResponsesReasoningMode(body, upstreamModel); err != nil {
			return nil, err
		} else if changed {
			body = normalized
		}
	}
	return body, nil
}
