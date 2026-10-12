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

type BorrowGatewayFeedbackRepository interface {
	BorrowGatewayPriorities(context.Context, BorrowAcquisition) (map[string]time.Time, error)
	RecordBorrowGatewayOutcome(context.Context, BorrowAcquisition, BorrowGatewayOutcome) error
}

func BorrowGatewayFeedbackReason(reason string) bool {
	return reason == "target_probe_passed" || reason == "target_ticket_changed" || reason == "target_route_changed"
}
