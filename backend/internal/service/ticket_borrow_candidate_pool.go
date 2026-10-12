package service

import (
	"context"
	"errors"
	"net/http"
	"time"
)

const (
	DefaultBorrowCandidatePoolCapacity   = 64
	BorrowCandidatePoolSourceConcurrency = 4
	// One short wave of prefetch, never an 80-second FIFO backlog. This is a
	// conservative policy bound, not an empirically optimal success threshold.
	BorrowCandidatePoolQueueLifetime  = 15 * time.Second
	BorrowCandidatePoolPublishReserve = 7 * time.Second
	BorrowCandidateVerificationLimit  = 90 * time.Second
	// A candidate claim covers bounded verification and publication/cleanup;
	// it never extends the target task lease or the candidate's admission cutoff.
	BorrowCandidatePoolClaimLease = BorrowCandidateVerificationLimit + 10*time.Second
)

var ErrBorrowCandidateTimeBudget = errors.New("candidate_time_budget_insufficient")
var ErrBorrowCandidatePoolWindow = errors.New("candidate_pool_window_too_short")

// The source task is independent of every target lease. ID is immutable and
// Lease fences a late source result after this task has finished or expired.
type BorrowPoolAcquisition struct {
	ID               string
	Lease            string
	SettingsRevision int64
	SourceID         int64
}
type borrowPoolAcquisitionKey struct{}

func WithBorrowPoolAcquisition(ctx context.Context, a BorrowPoolAcquisition) context.Context {
	return context.WithValue(ctx, borrowPoolAcquisitionKey{}, a)
}
func BorrowPoolAcquisitionFromContext(ctx context.Context) (BorrowPoolAcquisition, bool) {
	a, ok := ctx.Value(borrowPoolAcquisitionKey{}).(BorrowPoolAcquisition)
	return a, ok
}

// Only encrypted persistence may serialize this payload. Details remains the
// public allowlist; source credential hashes and the frozen environment are
// restored explicitly because Details intentionally does not serialize them.
type BorrowPoolCandidate struct {
	ID                   string
	SourceID             int64
	Cookie               http.Cookie
	Expires              time.Time
	Details              *BorrowCandidateDetails
	Environment          *BorrowCandidateEnvironment
	SourceSubjectHash    string
	SourceCredentialHash string
}

type BorrowCandidatePoolRepository interface {
	RegisterCandidatePoolDemand(context.Context, BorrowAcquisition, int) error
	RemoveCandidatePoolDemand(context.Context, BorrowAcquisition) error
	ReserveCandidatePoolFetch(context.Context, string, string) (*BorrowPoolAcquisition, error)
	CheckCandidatePoolAcquisition(context.Context, BorrowPoolAcquisition) error
	EnqueueCandidatePool(context.Context, BorrowPoolAcquisition, *BorrowPoolCandidate) error
	ClaimCandidatePool(context.Context, BorrowAcquisition) (*BorrowPoolCandidate, error)
	FinishCandidatePool(context.Context, string, string, string) error
	SweepCandidatePool(context.Context) error
	PublishedPoolCandidate(context.Context, BorrowAcquisition, string) (string, error)
}

func BorrowPoolVerificationBudget(continueSeconds int, phase string) time.Duration {
	if continueSeconds <= 0 {
		continueSeconds = 7
	}
	budget := time.Duration(continueSeconds)*time.Second + BorrowCandidatePoolPublishReserve
	if phase != "continue" {
		budget += openAICodexStateProbeShotTimeout
	}
	return budget
}

func BorrowPoolSettingsBudget(settings BorrowSettings, phase string) time.Duration {
	seconds := 7
	if settings.ContinueTimeoutSeconds != nil {
		seconds = *settings.ContinueTimeoutSeconds
	}
	return BorrowPoolVerificationBudget(seconds, phase)
}

type borrowPoolVerificationKey struct{}
type borrowPoolVerificationDeadline struct{ Until time.Time }

// Use the repository clock at each actual HTTP attempt. Stage budgets are
// recomputed after mint and retries never reset the original cutoff.
func (s *TicketBorrowService) checkPoolVerificationTime(ctx context.Context, phase string) error {
	v, ok := ctx.Value(borrowPoolVerificationKey{}).(borrowPoolVerificationDeadline)
	if !ok {
		return nil
	}
	cfg, err := s.repo.Settings(ctx)
	if err != nil {
		return err
	}
	now, err := s.repo.Now(ctx)
	if err != nil {
		return err
	}
	if v.Until.Sub(now) < BorrowPoolSettingsBudget(cfg, phase) {
		return ErrBorrowCandidateTimeBudget
	}
	return nil
}
