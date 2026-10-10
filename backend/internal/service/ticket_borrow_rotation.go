package service

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
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
func (s *TicketBorrowService) SaveRotationPolicy(ctx context.Context, id int64, p BorrowRotationPolicy) error {
	if id < 1 || p.Revision < 0 || p.IntervalSeconds < 1 || p.IntervalSeconds > 86400 {
		return ErrBorrowRotationInvalid
	}
	r, ok := s.repo.(BorrowRotationRepository)
	if !ok {
		return ErrBorrowUnavailable
	}
	return r.SaveRotationPolicy(ctx, id, p)
}

// Batch policies are independent saves. Keep each account's revision fence and
// report partial failures without exposing repository errors or retrying writes.
func (s *TicketBorrowService) SaveRotationPolicies(ctx context.Context, in BorrowRotationBatchInput) ([]BorrowRotationBatchResult, error) {
	if len(in.Accounts) == 0 || len(in.Accounts) > 1000 || (in.Automatic == nil && in.IntervalSeconds == nil) ||
		(in.IntervalSeconds != nil && (*in.IntervalSeconds < 1 || *in.IntervalSeconds > 86400)) {
		return nil, ErrBorrowRotationInvalid
	}
	seen := make(map[int64]bool, len(in.Accounts))
	for _, a := range in.Accounts {
		if a.AccountID < 1 || a.Revision == nil || *a.Revision < 0 || seen[a.AccountID] {
			return nil, ErrBorrowRotationInvalid
		}
		seen[a.AccountID] = true
	}
	states, err := s.RotationStates(ctx)
	if err != nil {
		return nil, err
	}
	policies := make(map[int64]BorrowRotationPolicy, len(states))
	for _, state := range states {
		policies[state.AccountID] = state.BorrowRotationPolicy
	}
	out := make([]BorrowRotationBatchResult, 0, len(in.Accounts))
	for _, a := range in.Accounts {
		p, exists := policies[a.AccountID]
		if !exists {
			p = BorrowRotationPolicy{IntervalSeconds: 60}
		}
		result := BorrowRotationBatchResult{AccountID: a.AccountID, Status: "failed"}
		if p.Revision != *a.Revision {
			result.Reason = "borrow_configuration_conflict"
			out = append(out, result)
			continue
		}
		if in.Automatic != nil {
			p.Automatic = *in.Automatic
		}
		if in.IntervalSeconds != nil {
			p.IntervalSeconds = *in.IntervalSeconds
		}
		err := s.SaveRotationPolicy(ctx, a.AccountID, p)
		switch {
		case err == nil:
			result.Status = "saved"
		case errors.Is(err, ErrBorrowConflict):
			result.Reason = "borrow_configuration_conflict"
		case errors.Is(err, ErrBorrowRotationInvalid):
			result.Reason = "borrow_rotation_invalid"
		default:
			result.Reason = "borrow_operation_failed"
		}
		out = append(out, result)
	}
	return out, nil
}

func (s *TicketBorrowService) Rotate(ctx context.Context, in BorrowRotationInput) (*BorrowRotationJob, error) {
	if _, e := uuid.Parse(in.ID); e != nil || in.Role != "source" || len(in.AccountIDs) == 0 || len(in.AccountIDs) > 1000 {
		return nil, ErrBorrowRotationInvalid
	}
	seen := map[int64]bool{}
	ids := []int64{}
	for _, id := range in.AccountIDs {
		if id < 1 {
			return nil, ErrBorrowRotationInvalid
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	in.AccountIDs = ids
	r, ok := s.repo.(BorrowRotationRepository)
	if !ok {
		return nil, ErrBorrowUnavailable
	}
	return r.QueueRotation(ctx, in, "manual")
}
func (s *TicketBorrowService) targetEnvironment(ctx context.Context, a *Account) (BorrowTargetEnvironment, string, error) {
	var env BorrowTargetEnvironment
	if r, ok := s.repo.(BorrowRotationRepository); ok {
		var e error
		env, e = r.TargetEnvironment(ctx, a.ID)
		if e != nil {
			return env, "", e
		}
	}
	if env.EntryID == 0 {
		return env, s.targetIdentity(a), nil
	}
	// The managed borrowing environment supersedes the ordinary proxy/TLS
	// configuration, while the authenticated account subject still fences routes.
	raw, _ := json.Marshal([]any{a.ID, a.Credentials["chatgpt_account_id"], a.Credentials["chatgpt_user_id"], env.EntryID, env.Version, env.DeviceID, env.Proxy, borrowSourceProfile(env.DeviceID)})
	return env, borrowHash(string(raw)), nil
}

// Legacy device-derived digests retained for pre-v2 bindings and audit history.
// Snapshot profiles use BorrowTLSFingerprintHashes with materialized defaults.
func BorrowFingerprintHashes(device string) (string, string) {
	profile := borrowSourceProfile(device)
	profile.Name = ""
	raw, _ := json.Marshal(profile)
	return borrowHash(device), borrowHash(string(raw))
}
