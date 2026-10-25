package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var ErrBorrowRotationInvalid = errors.New("borrow_rotation_invalid")
var ErrBorrowRotationNoProxy = errors.New("borrow_rotation_no_alternative")
var ErrBorrowRotationNoFingerprint = errors.New("borrow_rotation_fingerprint_unavailable")
var ErrBorrowRotationPending = errors.New("borrow_rotation_pending")

type BorrowRotationInput struct {
	ID         string  `json:"request_id"`
	Role       string  `json:"role"`
	AccountIDs []int64 `json:"account_ids"`
}
type BorrowRotationPolicy struct {
	Revision        int64 `json:"revision"`
	Automatic       bool  `json:"automatic"`
	IntervalSeconds int   `json:"interval_seconds"`
}
type BorrowRotationBatchAccount struct {
	AccountID int64  `json:"account_id"`
	Revision  *int64 `json:"revision"`
}
type BorrowRotationBatchInput struct {
	Accounts        []BorrowRotationBatchAccount `json:"accounts"`
	Automatic       *bool                        `json:"automatic"`
	IntervalSeconds *int                         `json:"interval_seconds"`
}
type BorrowRotationBatchResult struct {
	AccountID int64  `json:"account_id"`
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"`
}

type BorrowRotationState struct {
	BorrowRotationPolicy
	AccountID          int64      `json:"account_id"`
	Role               string     `json:"role"`
	SelectionKind      string     `json:"selection_kind"`
	NextAt             *time.Time `json:"next_at,omitempty"`
	LastAt             *time.Time `json:"last_at,omitempty"`
	Phase              string     `json:"phase"`
	Reason             string     `json:"reason"`
	ActiveJob          string     `json:"active_job,omitempty"`
	PoolEntryID        int64      `json:"pool_entry_id,omitempty"`
	PoolEndpoint       string     `json:"pool_endpoint,omitempty"`
	FingerprintID      string     `json:"fingerprint_id,omitempty"`
	EnvironmentVersion int64      `json:"environment_version"`
	ServerNow          time.Time  `json:"server_now"`
}
type BorrowRotationSkipped struct {
	AccountID int64  `json:"account_id"`
	Reason    string `json:"reason"`
}
type BorrowRotationJob struct {
	Skipped    []BorrowRotationSkipped `json:"skipped"`
	ID         string                  `json:"id"`
	Role       string                  `json:"role"`
	Trigger    string                  `json:"trigger"`
	AccountIDs []int64                 `json:"account_ids"`
	Phase      string                  `json:"phase"`
	Reason     string                  `json:"reason"`
}
type BorrowTargetEnvironment struct {
	EntryID  int64
	Version  int64
	DeviceID string
	Proxy    string
}

// Separate capability keeps older test doubles and non-SQL repositories usable.
type BorrowRotationRepository interface {
	RotationStates(context.Context) ([]BorrowRotationState, error)
	SaveRotationPolicy(context.Context, int64, BorrowRotationPolicy) error
	QueueRotation(context.Context, BorrowRotationInput, string) (*BorrowRotationJob, error)
	ProcessRotations(context.Context) error
	TargetEnvironment(context.Context, int64) (BorrowTargetEnvironment, error)
}

func (s *TicketBorrowService) RotationStates(ctx context.Context) ([]BorrowRotationState, error) {
	r, ok := s.repo.(BorrowRotationRepository)
	if !ok {
		return nil, ErrBorrowUnavailable
	}
	return r.RotationStates(ctx)
}
func (s *TicketBorrowService) SaveRotationPolicy(context.Context, int64, BorrowRotationPolicy) error {
	return ErrBorrowRotationRetired
}

// Legacy mutation endpoints cannot re-enable account-bound environments.
func (s *TicketBorrowService) SaveRotationPolicies(context.Context, BorrowRotationBatchInput) ([]BorrowRotationBatchResult, error) {
	return nil, ErrBorrowRotationRetired
}

func (s *TicketBorrowService) Rotate(context.Context, BorrowRotationInput) (*BorrowRotationJob, error) {
	return nil, ErrBorrowRotationRetired
}
func (s *TicketBorrowService) targetEnvironment(ctx context.Context, a *Account) (BorrowTargetEnvironment, string, error) {
	return BorrowTargetEnvironment{}, s.targetIdentity(a), nil
}

// Legacy device-derived digests retained for pre-v2 bindings and audit history.
// Snapshot profiles use BorrowTLSFingerprintHashes with materialized defaults.
func BorrowFingerprintHashes(device string) (string, string) {
	profile := borrowSourceProfile(device)
	profile.Name = ""
	raw, _ := json.Marshal(profile)
	return borrowHash(device), borrowHash(string(raw))
}
