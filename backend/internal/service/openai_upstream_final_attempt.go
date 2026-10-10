package service

import "context"

// A worker may finish a request without executing the customer retry loop.
// Keep the full normal error policy, but finalize transient 429 cooldown now.
type openAIUpstreamFinalAttemptKey struct{}

func openAIUpstreamFinalAttempt(ctx context.Context) bool {
	final, _ := ctx.Value(openAIUpstreamFinalAttemptKey{}).(bool)
	return final
}
