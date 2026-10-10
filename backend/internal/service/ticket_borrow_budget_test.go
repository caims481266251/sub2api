package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type borrowBudgetTestRepo struct {
	*borrowSignalRepo
	reservations int
	deny         error
}

func (r *borrowBudgetTestRepo) ReserveSourceAttempt(context.Context, int64, string) error {
	r.reservations++
	if r.deny != nil {
		return r.deny
	}
	if r.reservations > 1 {
		return &BorrowBudgetWaitError{Scope: "source", Until: time.Now().Add(40 * time.Second), RetryAfter: 40 * time.Second}
	}
	return nil
}

func TestTicketBorrowSourceReservationGatesAreNotMisreported(t *testing.T) {
	storage := errors.New("test storage failure")
	for _, tc := range []struct {
		name            string
		gate, errorWant error
		reason          string
		delay           time.Duration
	}{
		{"source", &BorrowBudgetWaitError{Scope: "source", RetryAfter: 40 * time.Second}, errBorrowSourceDeferred, "source_request_budget", 40 * time.Second},
		{"target", &BorrowBudgetWaitError{Scope: "target", RetryAfter: 40 * time.Second}, errBorrowSourceDeferred, "target_request_budget", 0},
		{"paused", ErrBorrowPaused, errBorrowSourceDeferred, "borrow_acquisition_paused", 0},
		{"upstream", ErrBorrowRecoveryWaiting, errBorrowSourceDeferred, "target_upstream_waiting", 0},
		{"storage", storage, storage, "source_store_unavailable", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, base := borrowFixture()
			r := &borrowBudgetTestRepo{deny: tc.gate, borrowSignalRepo: &borrowSignalRepo{borrowWorkerRepo: &borrowWorkerRepo{borrowTestRepo: base, source: boundBorrowTestSource(BorrowSource{ID: 1, Enabled: true, Model: "gpt-6-luna", Secret: BorrowSourceSecret{Credentials: map[string]any{"access_token": "local-test"}}})}}}
			s.repo = r
			u := &stateProbeUpstream{}
			s.owner.httpUpstream = u
			_, _, err := s.acquireSource(t.Context(), r.source, BorrowSettings{TTLSeconds: 230})
			require.ErrorIs(t, err, tc.errorWant)
			require.Empty(t, u.calls)
			require.Equal(t, tc.reason, r.reason)
			require.Equal(t, tc.delay, r.delay)
		})
	}
}
func TestTicketBorrowSourceBudgetCountsRetryAndStopsBeforeSend(t *testing.T) {
	s, base := borrowFixture()
	r := &borrowBudgetTestRepo{borrowSignalRepo: &borrowSignalRepo{borrowWorkerRepo: &borrowWorkerRepo{borrowTestRepo: base, source: boundBorrowTestSource(BorrowSource{ID: 1, Enabled: true, Model: "gpt-6-luna", Secret: BorrowSourceSecret{Credentials: map[string]any{"access_token": "local-test"}}})}}}
	s.repo = r
	u := &stateProbeUpstream{replies: []stateProbeReply{{status: 500, body: "unavailable"}}}
	s.owner.httpUpstream = u
	_, _, err := s.acquireSource(t.Context(), r.source, BorrowSettings{TTLSeconds: 230})
	require.ErrorIs(t, err, errBorrowSourceDeferred)
	require.Len(t, u.calls, 1)
	require.Equal(t, 2, r.reservations)
	require.Equal(t, "source_request_budget", r.reason)
	require.Equal(t, 40*time.Second, r.delay)
}
func TestTicketBorrowBudgetErrorStopsTargetRetries(t *testing.T) {
	calls := 0
	gate := &BorrowBudgetWaitError{Scope: "target", Until: time.Now().Add(time.Minute), RetryAfter: time.Minute}
	_, err := retryOpenAICodexStateRouteShot(t.Context(), &OpenAICodexStateProbeResult{}, true, func() (openAICodexStateShot, error) { calls++; return openAICodexStateShot{}, gate })
	require.ErrorIs(t, err, ErrBorrowRequestBudget)
	require.Equal(t, 1, calls)
}
