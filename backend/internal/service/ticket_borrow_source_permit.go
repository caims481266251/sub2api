package service

import (
	"context"
	"time"
)

// One ID per HTTP attempt. A successful grant owns both a source lease and a
// conservative budget reservation. Queueing owns neither.
type BorrowSourcePermitRepository interface {
	SourceHoldUntil(context.Context, int64) (*time.Time, error)
	QueueSourceAttempt(context.Context, int64, string) error
	GrantSourceAttempt(context.Context, int64, string) (bool, error)
	CancelSourceAttempt(context.Context, string) error
	SourceAttemptSent(context.Context, string) error
	SourceAttemptCompleted(context.Context, string) error
}
