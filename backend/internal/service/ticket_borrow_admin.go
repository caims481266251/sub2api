package service

import (
	"context"
	"errors"
	"time"
)

// Controls are saved independently from the advanced settings form. The saved
// revision still fences concurrent administrators, including a concurrent PUT.
type BorrowControls struct {
	Revision int64 `json:"revision"`
	Running  *bool `json:"running"`
}

func (s *TicketBorrowService) SaveControls(ctx context.Context, input BorrowControls) error {
	if input.Revision < 1 || input.Running == nil {
		return errors.New("borrow_settings_invalid")
	}
	v, err := s.repo.Settings(ctx)
	if err != nil {
		return err
	}
	if v.Revision != input.Revision {
		return ErrBorrowConflict
	}
	if input.Running != nil {
		v.Running = *input.Running
	}
	return s.repo.SaveSettings(ctx, v)
}

type BorrowAdminTargetRepository interface {
	AdminTargets(context.Context) ([]BorrowTarget, error)
}

type BorrowEventQuery struct {
	Before        int64
	After         int64
	TargetID      int64
	SourceAccount int64
	SourceID      int64
	Stage         string
	Reason        string
	CandidateID   string
	RouteID       string
	Search        string
	Since         time.Time
	Until         time.Time
	HideRotations bool
}

type BorrowEventQueryRepository interface {
	QueryEvents(context.Context, BorrowEventQuery) ([]BorrowEvent, error)
}

func (s *TicketBorrowService) QueryEvents(ctx context.Context, q BorrowEventQuery) ([]BorrowEvent, error) {
	if q.Before < 0 || q.After < 0 || q.TargetID < 0 || q.SourceAccount < 0 || q.SourceID < 0 ||
		(q.Before > 0 && q.After > 0) || len(q.Search) > 256 || len(q.Stage) > 80 || len(q.Reason) > 128 || len(q.RouteID) > 128 || len(q.CandidateID) > 128 ||
		(!q.Until.IsZero() && !q.Since.IsZero() && q.Until.Before(q.Since)) {
		return nil, errors.New("borrow_event_query_invalid")
	}
	r, ok := s.repo.(BorrowEventQueryRepository)
	if !ok {
		return nil, ErrBorrowUnavailable
	}
	return r.QueryEvents(ctx, q)
}
