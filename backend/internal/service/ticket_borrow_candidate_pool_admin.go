package service

import (
	"context"
	"errors"
	"strings"
	"time"
)

var (
	ErrBorrowPoolQuery    = errors.New("borrow_pool_query_invalid")
	ErrBorrowPoolCapacity = errors.New("borrow_pool_capacity_invalid")
	ErrBorrowPoolNotFound = errors.New("borrow_pool_candidate_not_found")
)

const BorrowCandidatePoolMaxCapacity = 1024

type BorrowCandidatePoolConfig struct {
	Capacity int   `json:"capacity"`
	Revision int64 `json:"revision"`
}

func (c BorrowCandidatePoolConfig) Validate() error {
	if c.Capacity < 1 || c.Capacity > BorrowCandidatePoolMaxCapacity || c.Revision < 1 {
		return ErrBorrowPoolCapacity
	}
	return nil
}

type BorrowCandidatePoolSummary struct {
	BorrowCandidatePoolConfig
	Running        bool `json:"running"`
	Acquiring      int  `json:"acquiring"`
	Queued         int  `json:"queued"`
	Claimed        int  `json:"claimed"`
	Occupied       int  `json:"occupied"`
	WaitingTargets int  `json:"waiting_targets"`
	DemandSlots    int  `json:"demand_slots"`
}

type BorrowCandidatePoolQuery struct {
	State           string
	SourceAccountID int64
	TargetID        int64
	Search          string
	Page            int
	PageSize        int
}

func (q *BorrowCandidatePoolQuery) Normalize() error {
	if q.State == "" {
		q.State = "active"
	}
	switch q.State {
	case "active", "all", "acquiring", "queued", "claimed", "finished":
	default:
		return ErrBorrowPoolQuery
	}
	if q.Page == 0 {
		q.Page = 1
	}
	if q.PageSize == 0 {
		q.PageSize = 50
	}
	q.Search = strings.TrimSpace(q.Search)
	if q.Page < 1 || q.Page > 1000000 || q.PageSize < 1 || q.PageSize > 100 || q.SourceAccountID < 0 || q.TargetID < 0 || len(q.Search) > 200 {
		return ErrBorrowPoolQuery
	}
	return nil
}

// Explicit public metadata only. No encrypted payloads or owner/claim tokens.
type BorrowCandidatePoolItem struct {
	ID              string     `json:"id"`
	State           string     `json:"state"`
	SourceID        int64      `json:"source_id"`
	SourceAccountID int64      `json:"source_account_id"`
	SourceName      string     `json:"source_name"`
	TargetID        int64      `json:"target_id"`
	TargetName      string     `json:"target_name"`
	Gateway         string     `json:"gateway"`
	CreatedAt       time.Time  `json:"created_at"`
	EnqueuedAt      *time.Time `json:"enqueued_at,omitempty"`
	ClaimedAt       *time.Time `json:"claimed_at,omitempty"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	AdmissionUntil  *time.Time `json:"admission_until,omitempty"`
	QueueUntil      *time.Time `json:"queue_until,omitempty"`
	Reason          string     `json:"reason"`
	RouteID         string     `json:"route_id,omitempty"`
}

type BorrowCandidatePoolPage struct {
	Summary   BorrowCandidatePoolSummary `json:"summary"`
	Items     []BorrowCandidatePoolItem  `json:"items"`
	Total     int                        `json:"total"`
	Page      int                        `json:"page"`
	PageSize  int                        `json:"page_size"`
	ServerNow time.Time                  `json:"server_now"`
}

type BorrowCandidatePoolDetail struct {
	BorrowCandidatePoolItem
	Details   *BorrowCandidateDetails `json:"details,omitempty"`
	ServerNow time.Time               `json:"server_now"`
}

type BorrowCandidatePoolAdminRepository interface {
	CandidatePoolPage(context.Context, BorrowCandidatePoolQuery) (*BorrowCandidatePoolPage, error)
	CandidatePoolDetail(context.Context, string) (*BorrowCandidatePoolDetail, error)
	SaveCandidatePoolConfig(context.Context, BorrowCandidatePoolConfig) (*BorrowCandidatePoolConfig, error)
}

func (s *TicketBorrowService) CandidatePoolPage(ctx context.Context, q BorrowCandidatePoolQuery) (*BorrowCandidatePoolPage, error) {
	if err := q.Normalize(); err != nil {
		return nil, err
	}
	r, ok := s.repo.(BorrowCandidatePoolAdminRepository)
	if !ok {
		return nil, ErrBorrowUnavailable
	}
	return r.CandidatePoolPage(ctx, q)
}

func (s *TicketBorrowService) CandidatePoolDetail(ctx context.Context, id string) (*BorrowCandidatePoolDetail, error) {
	if id == "" || len(id) > 200 {
		return nil, ErrBorrowPoolQuery
	}
	r, ok := s.repo.(BorrowCandidatePoolAdminRepository)
	if !ok {
		return nil, ErrBorrowUnavailable
	}
	return r.CandidatePoolDetail(ctx, id)
}

func (s *TicketBorrowService) SaveCandidatePoolConfig(ctx context.Context, config BorrowCandidatePoolConfig) (*BorrowCandidatePoolConfig, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	r, ok := s.repo.(BorrowCandidatePoolAdminRepository)
	if !ok {
		return nil, ErrBorrowUnavailable
	}
	return r.SaveCandidatePoolConfig(ctx, config)
}
