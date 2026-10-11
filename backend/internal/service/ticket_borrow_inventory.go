package service

import (
	"context"
	"errors"
	"time"
)

// Inventory facts exclude retired, historical validation-only and expired rows.
// The repository uses database time, both for admission and for the UI snapshot.
type BorrowInventoryFacts struct {
	Now                 time.Time
	Count               int
	EarliestExpiry      *time.Time
	MinRemainingSeconds int
	RenewBeforeSeconds  int
	LastStarted         *time.Time
	LastState           string
	RefreshGeneration   int64
	ProcessedGeneration int64
}

type BorrowInventory struct {
	Count              int        `json:"inventory_count"`
	Desired            int        `json:"desired_inventory"`
	State              string     `json:"maintenance_state"`
	NextAt             time.Time  `json:"next_probe_at"`
	EarliestAdmitUntil *time.Time `json:"earliest_admit_until,omitempty"`
	RefreshGeneration  int64      `json:"refresh_generation"`
	BlockedReason      string     `json:"blocked_reason,omitempty"`
	ServerNow          time.Time  `json:"-"`
	EarliestExpiry     *time.Time `json:"-"`
}

type BorrowInventoryRepository interface {
	Inventory(context.Context, int64, string) (*BorrowInventory, error)
	// Stage gating applies only before acquisition or before mint. It must never
	// block continuation or publication of a candidate already being verified.
	NeedsCandidate(context.Context, BorrowAcquisition, *time.Time) (bool, error)
}

func EvaluateBorrowInventory(f BorrowInventoryFacts, p *BorrowReplenishmentSettings) BorrowInventory {
	out := BorrowInventory{Count: f.Count, Desired: 1, State: "search", NextAt: f.Now, ServerNow: f.Now, RefreshGeneration: f.RefreshGeneration}
	enabled := p != nil && p.Enabled
	if enabled {
		out.Desired = p.Inventory
	}
	if f.EarliestExpiry != nil {
		out.EarliestExpiry = f.EarliestExpiry
		v := f.EarliestExpiry.Add(-time.Duration(f.MinRemainingSeconds) * time.Second)
		out.EarliestAdmitUntil = &v
	}
	switch {
	case f.Count == 0:
	case f.Count < out.Desired:
		out.State = "refill"
	case f.EarliestExpiry != nil && !f.EarliestExpiry.Add(-time.Duration(max(f.RenewBeforeSeconds, f.MinRemainingSeconds))*time.Second).After(f.Now):
		out.State = "renew"
	default:
		out.State = "healthy"
	}
	if !enabled {
		if out.State == "healthy" && f.EarliestExpiry != nil {
			out.NextAt = f.EarliestExpiry.Add(-time.Duration(max(f.RenewBeforeSeconds, f.MinRemainingSeconds)) * time.Second)
		}
		return out
	}
	interval := 1
	switch out.State {
	case "refill", "renew":
		interval = p.IntervalSeconds
	case "healthy":
		interval = p.MaintenanceSeconds()
	}
	if f.LastStarted != nil {
		out.NextAt = f.LastStarted.Add(time.Duration(interval) * time.Second)
		// A loss of inventory is a new need; repeated failure in the same state is not.
		if f.LastState == "healthy" && out.State != "healthy" {
			out.NextAt = f.Now
		}
	}
	if out.State == "healthy" && f.EarliestExpiry != nil {
		renew := f.EarliestExpiry.Add(-time.Duration(max(f.RenewBeforeSeconds, f.MinRemainingSeconds)) * time.Second)
		if renew.Before(out.NextAt) {
			out.NextAt = renew
		}
	}
	if f.RefreshGeneration > f.ProcessedGeneration {
		out.NextAt = f.Now
	}
	return out
}

type BorrowCandidateRepository interface {
	RecordCandidate(context.Context, BorrowAcquisition, string, string, string, string, time.Time) error
}

var ErrBorrowCandidateDuplicate = errors.New("candidate_duplicate")
var ErrBorrowCandidateExpiry = errors.New("candidate_publish_expired")
