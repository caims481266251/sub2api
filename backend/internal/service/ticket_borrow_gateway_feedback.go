package service

import (
	"context"
	"time"
)

// Feedback orders fresh candidates. It never grants business eligibility or
// rejects a candidate solely because another Cookie on its gateway failed.
type BorrowGatewayOutcome struct {
	CandidateID string
	CookieHash  string
	Gateway     string
	StartedAt   time.Time
	Reason      string
}

// Counts describe the candidates actually available at selection time. An
// unchanged choice with no alternative must not be reported as failed ranking.
type BorrowGatewaySelection struct {
	Eligible      int `json:"eligible"`
	Gateways      int `json:"gateways"`
	Deprioritized int `json:"deprioritized"`
}

type BorrowGatewayFeedbackRepository interface {
	BorrowGatewayPriorities(context.Context, BorrowAcquisition) (map[string]time.Time, error)
	RecordBorrowGatewayOutcome(context.Context, BorrowAcquisition, BorrowGatewayOutcome) error
}

func BorrowGatewayFeedbackReason(reason string) bool {
	return reason == "target_probe_passed" || reason == "target_ticket_changed" || reason == "target_route_changed"
}

// The shared pool follows the same existing emergency disable as the direct
// pipeline; this context only affects ordering, never candidate eligibility.
type borrowGatewayFeedbackDisabledKey struct{}

func BorrowGatewayFeedbackEnabled(ctx context.Context) bool {
	disabled, _ := ctx.Value(borrowGatewayFeedbackDisabledKey{}).(bool)
	return !disabled
}
