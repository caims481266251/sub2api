package requesttiming

import "context"

// SessionAffinitySnapshot explains scheduling without containing raw session
// identifiers, prompts, response ownership data, or credentials.
type SessionAffinitySnapshot struct {
	Decision           string `json:"decision"`
	Reason             string `json:"reason"`
	OwnerAccountID     int64  `json:"owner_account_id"`
	ExecutionAccountID int64  `json:"execution_account_id"`
	OwnerPending       bool   `json:"owner_pending"`
	Success            bool   `json:"success"`
	OwnerCommitted     bool   `json:"owner_committed"`
	CacheUnavailable   bool   `json:"cache_unavailable"`
}

func SessionAffinity(ctx context.Context, state SessionAffinitySnapshot) {
	c := From(ctx)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.finished {
		c.data.SessionAffinity = &state
	}
}
