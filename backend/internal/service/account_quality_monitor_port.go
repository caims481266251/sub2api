package service

import (
	"context"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var (
	ErrAccountQualityScopeChanged = infraerrors.New(409, "ACCOUNT_QUALITY_SCOPE_CHANGED", "account identity changed; reload the account")
	ErrAccountQualityBusy         = infraerrors.New(429, "ACCOUNT_QUALITY_BUSY", "quality detection capacity is full; try again shortly")
	ErrAccountQualityUnavailable  = infraerrors.New(409, "ACCOUNT_QUALITY_UNAVAILABLE", "account is temporarily unavailable for quality detection")
	ErrAccountQualityStopped      = infraerrors.New(503, "ACCOUNT_QUALITY_STOPPED", "quality monitor is stopping")
	ErrAccountQualityTextModel    = infraerrors.BadRequest("ACCOUNT_QUALITY_TEXT_MODEL", "quality detection requires a text generation model")
)

type AccountQualityMonitorConfig struct {
	Scope           string `json:"scope"`
	Enabled         bool   `json:"enabled"`
	Model           string `json:"model"`
	IntervalMinutes int    `json:"interval_minutes"`
}

type AccountQualityMonitorResult struct {
	State      string    `json:"state"`
	Answer     string    `json:"answer,omitempty"`
	Model      string    `json:"model"`
	CheckedAt  time.Time `json:"checked_at"`
	DurationMS int64     `json:"duration_ms"`
	Error      string    `json:"error,omitempty"`
	Text       string    `json:"text,omitempty"`
}

type AccountQualityMonitorView struct {
	AccountQualityMonitorConfig
	AccountID     int64                        `json:"account_id"`
	State         string                       `json:"state"`
	PauseReason   string                       `json:"pause_reason,omitempty"`
	NextDueAt     *time.Time                   `json:"next_due_at,omitempty"`
	LatestAttempt *AccountQualityMonitorResult `json:"latest_attempt,omitempty"`
	LastValid     *AccountQualityMonitorResult `json:"last_valid,omitempty"`
}

type AccountQualityMonitorRecord struct {
	AccountQualityMonitorConfig
	AccountID     int64
	ResultVersion int64
	NextDueAt     *time.Time
	RunID         string
	LeaseUntil    *time.Time
	LatestAttempt *AccountQualityMonitorResult
	LastValid     *AccountQualityMonitorResult
}

type AccountQualityMonitorRepository interface {
	Get(context.Context, int64, bool) (*AccountQualityMonitorRecord, error)
	Batch(context.Context, []int64) (map[int64]*AccountQualityMonitorRecord, error)
	Save(context.Context, int64, AccountQualityMonitorConfig, time.Time) (*AccountQualityMonitorRecord, error)
	Ensure(context.Context, int64, AccountQualityMonitorConfig, time.Time) (*AccountQualityMonitorRecord, error)
	ListDue(context.Context, time.Time, int) ([]*AccountQualityMonitorRecord, error)
	Claim(context.Context, *AccountQualityMonitorRecord, string, time.Time, time.Time, bool) (bool, error)
	Renew(context.Context, int64, string, time.Time) (bool, error)
	Finish(context.Context, *AccountQualityMonitorRecord, string, *AccountQualityMonitorResult, time.Time) (bool, error)
	Release(context.Context, int64, string) error
	Postpone(context.Context, *AccountQualityMonitorRecord, time.Time) error
}
