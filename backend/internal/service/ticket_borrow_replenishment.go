package service

import (
	"context"
	"errors"
	"time"
)

// The global policy applies to every enabled target. Nil on writes preserves
// the saved policy for older clients. Values remain bounded trial parameters.
type BorrowReplenishmentSettings struct {
	Enabled                    bool `json:"enabled"`
	Inventory                  int  `json:"inventory"`
	IntervalSeconds            int  `json:"interval_seconds"`
	HoldSeconds                int  `json:"hold_seconds"`
	MaintenanceIntervalSeconds *int `json:"maintenance_interval_seconds,omitempty"`
	// Decode legacy input only; true is rejected, false is omitted on reads.
	Shadow bool `json:"shadow,omitempty"`
}

func (p *BorrowReplenishmentSettings) Allows(id int64) bool {
	return p != nil && p.Enabled && id > 0
}
func (p *BorrowReplenishmentSettings) Validate() error {
	if p == nil {
		return nil
	}
	if p.Shadow {
		return errors.New("borrow_shadow_unsupported")
	}
	if p.MaintenanceIntervalSeconds != nil && (*p.MaintenanceIntervalSeconds < 10 || *p.MaintenanceIntervalSeconds > 300 || *p.MaintenanceIntervalSeconds < p.IntervalSeconds) {
		return errors.New("borrow_settings_invalid")
	}
	if p.Inventory < 1 || p.Inventory > 4 || p.IntervalSeconds < 1 || p.IntervalSeconds > 60 || p.HoldSeconds < 1 || p.HoldSeconds > 150 {
		return errors.New("borrow_settings_invalid")
	}
	return nil
}

type BorrowReplenishmentState struct {
	SourceID           int64     `json:"source_id"`
	SourceAccountID    int64     `json:"source_account_id"`
	EnvironmentVersion int64     `json:"environment_version"`
	Until              time.Time `json:"until"`
	NextAt             time.Time `json:"next_at"`
	IntervalSeconds    int       `json:"interval_seconds"`
	Attempts           int64     `json:"attempts"`
}
type BorrowReplenishmentRepository interface {
	Replenishment(context.Context, int64, string) (*BorrowReplenishmentState, error)
	EndReplenishment(context.Context, BorrowAcquisition, int64, string) error
}

// Keep one acquisition worker dedicated to the retained source. The other
// workers explore without waiting for that source, and cannot steal its slot.
func borrowReplenishmentBatch(batch []BorrowSource, state *BorrowReplenishmentState, concurrency int) []BorrowSource {
	if state == nil {
		return batch
	}
	for i, source := range batch {
		if source.ID == state.SourceID {
			if concurrency == 1 && state.Attempts%2 == 0 {
				out := append([]BorrowSource{}, batch[:i]...)
				out = append(out, batch[i+1:]...)
				return append(out, source)
			}
			out := append([]BorrowSource{source}, batch[:i]...)
			return append(out, batch[i+1:]...)
		}
	}
	return batch
}

// No credentials or proxy URLs: lifecycle events are safe to export with the
// ordinary candidate evidence stream.
type BorrowReplenishmentAudit struct {
	EnvironmentVersion int64      `json:"environment_version,omitempty"`
	Until              *time.Time `json:"until,omitempty"`
	Inventory          int        `json:"inventory,omitempty"`
	ExitReason         string     `json:"exit_reason,omitempty"`
}

func borrowSubjectHash(a *Account) string {
	return borrowHash(borrowCredential(a.Credentials, "chatgpt_account_id") + ":" + borrowCredential(a.Credentials, "chatgpt_user_id"))
}

func (p *BorrowReplenishmentSettings) MaintenanceSeconds() int {
	if p.MaintenanceIntervalSeconds != nil {
		return *p.MaintenanceIntervalSeconds
	}
	return max(30, p.IntervalSeconds)
}
