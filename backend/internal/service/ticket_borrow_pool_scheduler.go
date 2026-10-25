package service

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

// The application scheduler owns producers; a target never lends its lease to
// source acquisition. Database reservations enforce the same limits across
// processes. The local count only avoids launching unnecessary goroutines.
func (s *TicketBorrowService) startPoolProducers(ctx context.Context) {
	pool, ok := s.repo.(BorrowCandidatePoolRepository)
	if !ok || s.poolSources.Load() >= BorrowCandidatePoolSourceConcurrency {
		return
	}
	// Sources synchronizes the configured selection into the source inventory.
	if _, err := s.repo.Sources(ctx); err != nil {
		return
	}
	for ctx.Err() == nil && s.poolSources.Load() < BorrowCandidatePoolSourceConcurrency {
		a, err := pool.ReserveCandidatePoolFetch(ctx, uuid.NewString(), uuid.NewString())
		if err != nil || a == nil {
			return
		}
		s.poolSources.Add(1)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.poolSources.Add(-1)
			s.producePoolCandidate(ctx, pool, *a)
		}()
	}
}

func (s *TicketBorrowService) producePoolCandidate(parent context.Context, pool BorrowCandidatePoolRepository, a BorrowPoolAcquisition) {
	ctx, cancel := context.WithTimeoutCause(WithBorrowPoolAcquisition(parent, a), 45*time.Second, borrowCancelCandidateDeadline)
	defer cancel()
	reason, queued := "candidate_pool_source_failed", false
	defer func() {
		if queued {
			return
		}
		if err := borrowCleanup(parent, func(cleanup context.Context) error { return pool.FinishCandidatePool(cleanup, a.ID, a.Lease, reason) }); err != nil {
			slog.Warn("borrow_pool_source_cleanup_failed", "candidate_id", a.ID)
		}
	}()
	source, err := s.repo.Source(ctx, a.SourceID)
	if err != nil || source == nil {
		return
	}
	settings, err := s.repo.Settings(ctx)
	if err != nil || !settings.Running || settings.Revision != a.SettingsRevision {
		reason = "candidate_pool_configuration_changed"
		return
	}
	details := &BorrowCandidateDetails{CandidateID: a.ID, BatchID: a.Lease}
	cookie, expires, err := s.acquireSourceObserved(ctx, *source, settings, details)
	if err != nil {
		if errors.Is(err, errBorrowSourceDeferred) {
			reason = "candidate_pool_source_deferred"
		}
		return
	}
	c := &BorrowPoolCandidate{ID: a.ID, SourceID: a.SourceID, Cookie: *cookie, Expires: expires,
		Details: details, Environment: details.Environment,
		SourceSubjectHash: details.SourceSubjectHash, SourceCredentialHash: details.SourceCredentialHash}
	if err = pool.EnqueueCandidatePool(ctx, a, c); err != nil {
		switch {
		case errors.Is(err, ErrBorrowCandidateTimeBudget):
			reason = "candidate_time_budget_insufficient"
		case errors.Is(err, ErrBorrowCandidateDuplicate):
			reason = "candidate_duplicate"
		case errors.Is(err, ErrBorrowUnavailable):
			reason = "candidate_pool_no_consumers"
		default:
			reason = "candidate_store_unavailable"
		}
		return
	}
	queued = true
}

func (s *TicketBorrowService) restorePooledCandidate(c *BorrowPoolCandidate) borrowProbeCandidate {
	d := c.Details
	d.Environment = c.Environment
	d.SourceSubjectHash, d.SourceCredentialHash = c.SourceSubjectHash, c.SourceCredentialHash
	d.checkSource = func(ctx context.Context) error {
		source, err := s.repo.Source(ctx, c.SourceID)
		if err != nil || source == nil || !source.Enabled || source.Revision != d.SourceRevision || source.AccountID != d.SourceAccountID {
			return ErrBorrowUnavailable
		}
		account, err := s.normalSourceAccount(ctx, *source)
		if err != nil || account == nil || borrowSubjectHash(account) != d.SourceSubjectHash || borrowHash(borrowCredential(account.Credentials, "access_token")) != d.SourceCredentialHash {
			return ErrBorrowUnavailable
		}
		return nil
	}
	return borrowProbeCandidate{source: c.SourceID, cookie: &c.Cookie, expires: c.Expires, details: d}
}

// Closing demand stops new claims, not the verification already admitted by
// this batch. Every started result reaches the normal publication callback and
// then a fenced pool terminal transition, even when another result filled the
// inventory first. The buffered channel permits bounded cancellation/draining.
func (s *TicketBorrowService) consumeCandidatePool(ctx context.Context, pool BorrowCandidatePoolRepository, a BorrowAcquisition, capacity int,
	needed func() (bool, error), verify func(borrowProbeCandidate) borrowProbeVerification,
	consume func(borrowProbeVerification) (bool, string)) string {
	// Target verification concurrency is separate from global pool occupancy.
	capacity = max(1, capacity)
	results := make(chan borrowProbeVerification, capacity)
	var workers sync.WaitGroup
	active, closed := 0, false
	closeDemand := func() {
		if closed {
			return
		}
		closed = true
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer stop()
		if err := pool.RemoveCandidatePoolDemand(cleanup, a); err != nil {
			slog.Warn("borrow_pool_demand_cleanup_failed", "target_id", a.TargetID)
		}
	}
	handle := func(out borrowProbeVerification) {
		active--
		keep, reason := consume(out)
		if err := borrowCleanup(ctx, func(cleanup context.Context) error {
			return pool.FinishCandidatePool(cleanup, out.candidate.details.CandidateID, a.Lease, reason)
		}); err != nil {
			slog.Warn("borrow_pool_claim_cleanup_failed", "candidate_id", out.candidate.details.CandidateID, "target_id", a.TargetID)
		}
		if !keep {
			closeDemand()
		}
	}
	defer func() {
		closeDemand()
		workers.Wait()
		for active > 0 {
			handle(<-results)
		}
	}()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := waitBorrowTargetLease(ctx); err != nil {
			return borrowAcquisitionReason(err)
		}
		if ctx.Err() != nil {
			return ""
		}
		for len(results) > 0 {
			handle(<-results)
		}
		if !closed {
			need, err := needed()
			if err != nil {
				return borrowAcquisitionReason(err)
			}
			if !need {
				closeDemand()
			} else if err = pool.RegisterCandidatePoolDemand(ctx, a, capacity); err != nil {
				return borrowAcquisitionReason(err)
			}
		}
		for !closed && active < capacity {
			if err := waitBorrowTargetLease(ctx); err != nil {
				return borrowAcquisitionReason(err)
			}
			c, err := pool.ClaimCandidatePool(ctx, a)
			if err != nil {
				return borrowAcquisitionReason(err)
			}
			if c == nil {
				break
			}
			item := s.restorePooledCandidate(c)
			active++
			workers.Add(1)
			go func() {
				defer workers.Done()
				results <- verify(item)
			}()
		}
		if closed && active == 0 {
			return ""
		}
		select {
		case <-ctx.Done():
			return ""
		case result := <-results:
			handle(result)
		case <-tick.C:
		}
	}
}
