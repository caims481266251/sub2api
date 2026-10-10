package service

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

var ErrBorrowUnavailable error = &OpenAITurnAdmissionError{Reason: "borrow_route_unavailable"}
var ErrBorrowConflict = errors.New("borrow_configuration_conflict")

type BorrowSourceSecret struct {
	Credentials map[string]any          `json:"credentials"`
	Proxies     []string                `json:"proxies"`
	DeviceID    string                  `json:"device_id"`
	Profile     *tlsfingerprint.Profile `json:"profile,omitempty"`
	Fingerprint *BorrowFingerprint      `json:"fingerprint,omitempty"`
}
type BorrowSelection struct {
	UseProxyPool   bool             `json:"use_proxy_pool"`
	Revision       int64            `json:"revision"`
	GroupIDs       []int64          `json:"group_ids"`
	AccountIDs     []int64          `json:"account_ids"`
	DefaultModel   string           `json:"default_model"`
	RotateIdentity bool             `json:"rotate_identity"`
	ModelOverrides map[int64]string `json:"model_overrides"`
}
type BorrowSourceOption struct {
	RequestEligible   bool    `json:"request_eligible"`
	UnavailableReason string  `json:"unavailable_reason,omitempty"`
	AuthExcluded      bool    `json:"auth_excluded"`
	ID                int64   `json:"id"`
	Name              string  `json:"name"`
	GroupIDs          []int64 `json:"group_ids"`
	Status            string  `json:"status"`
	Schedulable       bool    `json:"schedulable"`
	IsTarget          bool    `json:"is_target"`
}
type BorrowGroupOption struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}
type BorrowSourceOptions struct {
	Accounts        []BorrowSourceOption `json:"accounts"`
	Groups          []BorrowGroupOption  `json:"groups"`
	LegacyCount     int                  `json:"legacy_count"`
	RestorableCount int                  `json:"restorable_count"`
}
type BorrowSource struct {
	CanRotate                 bool               `json:"can_rotate"`
	RotationUnavailableReason string             `json:"rotation_unavailable_reason"`
	LeaseUntil                *time.Time         `json:"-"`
	RateLimitResetAt          *time.Time         `json:"rate_limit_reset_at,omitempty"`
	UseProxyPool              bool               `json:"use_proxy_pool"`
	PoolEntryID               int64              `json:"pool_entry_id,omitempty"`
	PoolEndpoint              string             `json:"pool_endpoint,omitempty"`
	FingerprintID             string             `json:"fingerprint_id,omitempty"`
	AccountID                 int64              `json:"account_id"`
	GroupIDs                  []int64            `json:"selected_group_ids"`
	Explicit                  bool               `json:"explicit"`
	UnavailableReason         string             `json:"unavailable_reason"`
	ID                        int64              `json:"id"`
	Name                      string             `json:"name"`
	Model                     string             `json:"model"`
	Enabled                   bool               `json:"enabled"`
	Revision                  int64              `json:"revision"`
	RotateIdentity            bool               `json:"rotate_identity"`
	ProxyCount                int                `json:"proxy_count"`
	Cursor                    int64              `json:"-"`
	LastReason                string             `json:"last_reason"`
	NextAttemptAt             time.Time          `json:"next_attempt_at"`
	Secret                    BorrowSourceSecret `json:"-"`
	SubjectHash               string             `json:"-"`
}
type BorrowSourceInput struct {
	Name           string         `json:"name"`
	Model          string         `json:"model"`
	Enabled        bool           `json:"enabled"`
	Revision       int64          `json:"revision"`
	RotateIdentity bool           `json:"rotate_identity"`
	Credentials    map[string]any `json:"credentials,omitempty"`
	Proxies        *[]string      `json:"proxies,omitempty"`
}
type BorrowRecoverySettings struct {
	// Deprecated recovery switches remain readable for older clients. Only the
	// independent request limits below affect acquisition.
	Enabled                 bool  `json:"enabled"`
	FailureThreshold        int   `json:"failure_threshold"`
	CooldownSeconds         []int `json:"cooldown_seconds"`
	ProbeRequestsPerMinute  int   `json:"probe_requests_per_minute"`
	SourceRequestsPerMinute int   `json:"source_requests_per_minute"`
}

func DefaultBorrowRecoverySettings() *BorrowRecoverySettings {
	return &BorrowRecoverySettings{CooldownSeconds: []int{}, ProbeRequestsPerMinute: 12, SourceRequestsPerMinute: 12}
}

// NormalizeBorrowSettings reports the policy actually enforced by this version.
// Legacy global prewarming and progressive cooldown stay disabled; renewal is
// controlled by the global probe policy with optional account overrides.
func NormalizeBorrowSettings(s *BorrowSettings) {
	s.PrewarmSeconds = 0
	if s.Recovery == nil {
		s.Recovery = DefaultBorrowRecoverySettings()
	}
	r := *s.Recovery
	r.Enabled, r.FailureThreshold, r.CooldownSeconds = false, 0, []int{}
	if r.ProbeRequestsPerMinute == 0 {
		r.ProbeRequestsPerMinute = 12
	}
	if r.SourceRequestsPerMinute == 0 {
		r.SourceRequestsPerMinute = 12
	}
	s.Recovery = &r
}

type BorrowAcquisition struct {
	TargetID int64
	Revision int64
	Lease    string
	Identity string
}

type borrowAcquisitionKey struct{}

func WithBorrowAcquisition(ctx context.Context, a BorrowAcquisition) context.Context {
	return context.WithValue(ctx, borrowAcquisitionKey{}, a)
}
func BorrowAcquisitionFromContext(ctx context.Context) (BorrowAcquisition, bool) {
	a, ok := ctx.Value(borrowAcquisitionKey{}).(BorrowAcquisition)
	return a, ok
}

type BorrowRequest struct {
	Identity    string             `json:"-"`
	ID          string             `json:"id"`
	TargetID    int64              `json:"target_id"`
	Kind        string             `json:"kind"`
	BatchID     string             `json:"batch_id"`
	SourceID    int64              `json:"source_id"`
	Phase       string             `json:"phase"`
	Revision    int64              `json:"-"`
	CandidateID string             `json:"candidate_id,omitempty"`
	RouteID     string             `json:"route_id,omitempty"`
	Environment *BorrowEnvironment `json:"environment,omitempty"`
}

// BorrowEnvironment is an explicit allowlist of diagnostic fields. Raw request
// headers, payloads, credentials, proxies, Cookies and tickets never belong here.
type BorrowEnvironment struct {
	TLSConfigHash        string `json:"tls_config_hash,omitempty"`
	TLSStructureHash     string `json:"tls_structure_hash,omitempty"`
	FingerprintGenerator string `json:"fingerprint_generator,omitempty"`
	IdentityHash         string `json:"identity_hash,omitempty"`
	ProfileHash          string `json:"profile_hash,omitempty"`
	ProxyHash            string `json:"configured_proxy_hash,omitempty"`
	ClientVersion        string `json:"client_version,omitempty"`
	BodyHash             string `json:"body_hash,omitempty"`
}

type BorrowCandidateDetails struct {
	Rotation               *BorrowRotationAudit `json:"rotation,omitempty"`
	CandidateID            string               `json:"candidate_id"`
	BatchID                string               `json:"batch_id"`
	SourceAccountID        int64                `json:"source_account_id,omitempty"`
	SourceModel            string               `json:"source_model,omitempty"`
	SourceFormat           string               `json:"source_format,omitempty"`
	SourceRevision         int64                `json:"source_revision,omitempty"`
	TargetRevision         int64                `json:"target_revision,omitempty"`
	Source                 BorrowEnvironment    `json:"source"`
	Target                 BorrowEnvironment    `json:"target"`
	TargetIdentityHash     string               `json:"target_identity_hash,omitempty"`
	CookieHash             string               `json:"cookie_hash,omitempty"`
	Gateway                string               `json:"gateway,omitempty"`
	CookieObservedAt       *time.Time           `json:"cookie_observed_at,omitempty"`
	ExpiresAt              *time.Time           `json:"expires_at,omitempty"`
	CookieAgeMS            *int64               `json:"cookie_age_ms,omitempty"`
	RemainingMS            *int64               `json:"remaining_ms,omitempty"`
	VerificationPreparedAt *time.Time           `json:"verification_prepared_at,omitempty"`
	CookieAgeAtProbeMS     *int64               `json:"cookie_age_at_probe_ms,omitempty"`
	RemainingAtProbeMS     *int64               `json:"remaining_at_probe_ms,omitempty"`
	SourceAttempts         int                  `json:"source_attempts,omitempty"`
	ProbeRetries           int                  `json:"probe_retries,omitempty"`
	ProbeVerdict           string               `json:"probe_verdict,omitempty"`
	ProbeFailure           string               `json:"probe_failure,omitempty"`
	MintStatus             int                  `json:"mint_status,omitempty"`
	ContinueStatus         int                  `json:"continue_status,omitempty"`
}
type BorrowSettings struct {
	// Nil on writes preserves the saved value for older API clients.
	MinRouteRemainingSeconds *int                    `json:"min_route_remaining_seconds,omitempty"`
	ProbeConcurrency         *int                    `json:"probe_concurrency,omitempty"`
	RenewBeforeSeconds       *int                    `json:"renew_before_seconds,omitempty"`
	SourceBudget             *BorrowBudgetStatus     `json:"source_budget,omitempty"`
	ServerNow                time.Time               `json:"server_now"`
	Recovery                 *BorrowRecoverySettings `json:"recovery,omitempty"`
	Revision                 int64                   `json:"revision"`
	Running                  bool                    `json:"running"`
	TTLSeconds               int                     `json:"ttl_seconds"`
	PrewarmSeconds           int                     `json:"prewarm_seconds"`
}

// Budget status is calculated from database reservations, never from client input.
type BorrowBudgetStatus struct {
	Used        int        `json:"used"`
	Limit       int        `json:"limit"`
	AvailableAt *time.Time `json:"available_at,omitempty"`
}
type BorrowTarget struct {
	ProbePolicy             BorrowProbePolicy `json:"probe_policy"`
	UpstreamWaitUntil       *time.Time        `json:"upstream_wait_until,omitempty"`
	UpstreamWaitReason      string            `json:"upstream_wait_reason,omitempty"`
	BudgetWaitUntil         *time.Time        `json:"budget_wait_until,omitempty"`
	CooldownUntil           *time.Time        `json:"cooldown_until,omitempty"`
	CooldownSeconds         int               `json:"cooldown_seconds"`
	RecoveryLevel           int               `json:"recovery_level"`
	ConsecutiveFailures     int               `json:"consecutive_failures"`
	RecoveryEpoch           int64             `json:"recovery_epoch"`
	LastRequestStartedAt    *time.Time        `json:"last_request_started_at,omitempty"`
	LastRequestFinishedAt   *time.Time        `json:"last_request_finished_at,omitempty"`
	Inflight                int               `json:"inflight"`
	ProbeRequestsLastMinute int               `json:"probe_requests_last_minute"`
	AccountID               int64             `json:"account_id"`
	Name                    string            `json:"name"`
	Enabled                 bool              `json:"enabled"`
	Revision                int64             `json:"revision"`
	Cursor                  int64             `json:"-"`
	ScanSources             []int64           `json:"-"`
	ScanOffset              int               `json:"scan_completed"`
	ScanTotal               int               `json:"scan_total"`
	RefreshRequested        bool              `json:"refresh_requested"`
	Phase                   string            `json:"phase"`
	Reason                  string            `json:"reason"`
	NextAttemptAt           time.Time         `json:"next_attempt_at"`
	Ready                   bool              `json:"ready"`
	Gateway                 string            `json:"gateway,omitempty"`
	ExpiresAt               *time.Time        `json:"expires_at,omitempty"`
	UsableUntil             *time.Time        `json:"usable_until,omitempty"`
	ServerNow               time.Time         `json:"server_now"`
}
type BorrowRouteSecret struct {
	Endpoint string                  `json:"endpoint"`
	Cookie   http.Cookie             `json:"cookie"`
	Headers  http.Header             `json:"headers"`
	Proxy    string                  `json:"proxy"`
	Profile  *tlsfingerprint.Profile `json:"profile,omitempty"`
}
type BorrowRoute struct {
	ID             string            `json:"id"`
	TargetID       int64             `json:"target_id"`
	TargetRevision int64             `json:"target_revision"`
	SourceID       int64             `json:"source_id"`
	IdentityHash   string            `json:"identity_hash"`
	CookieHash     string            `json:"cookie_hash"`
	Gateway        string            `json:"gateway"`
	ExpiresAt      time.Time         `json:"expires_at"`
	UsableUntil    time.Time         `json:"usable_until"`
	Secret         BorrowRouteSecret `json:"-"`
}
type BorrowEvent struct {
	Details         *BorrowCandidateDetails `json:"details,omitempty"`
	SourceAccountID int64                   `json:"source_account_id"`
	ID              int64                   `json:"id"`
	TargetID        int64                   `json:"target_id"`
	SourceID        int64                   `json:"source_id"`
	RouteID         string                  `json:"route_id"`
	Stage           string                  `json:"stage"`
	Reason          string                  `json:"reason"`
	CreatedAt       time.Time               `json:"created_at"`
}

// Persistent policy and route admission share one primary database snapshot.
// An absent policy is disabled only after a successful database read.
type TicketBorrowRepository interface {
	CheckAcquisition(context.Context, BorrowAcquisition) error
	ReserveSourceAttempt(context.Context, int64, string) error
	BeginTargetRequest(context.Context, BorrowRequest) error
	CheckTargetRequest(context.Context, BorrowRequest) error
	MarkTargetRequestSent(context.Context, BorrowRequest) error
	RenewTargetRequest(context.Context, BorrowRequest) error
	EndTargetRequest(context.Context, BorrowRequest, int, string) error
	RecordProbeOutcome(context.Context, int64, string, string, time.Duration) error
	AdvanceScan(context.Context, int64, string, int) error
	Sources(context.Context) ([]BorrowSource, error)
	Selection(context.Context) (BorrowSelection, error)
	SaveSelection(context.Context, BorrowSelection) error
	SourceOptions(context.Context) (BorrowSourceOptions, error)
	RestoreSources(context.Context) ([]int64, error)
	Source(context.Context, int64) (*BorrowSource, error)
	SaveSource(context.Context, *BorrowSource) error
	TransferSource(context.Context, *Account, *BorrowSource) error
	RefreshSource(context.Context, int64, int64, string, BorrowSourceSecret) error
	DeleteSource(context.Context, int64, int64) error
	Settings(context.Context) (BorrowSettings, error)
	SaveSettings(context.Context, BorrowSettings) error
	Targets(context.Context) ([]BorrowTarget, error)
	Target(context.Context, int64) (BorrowTarget, error)
	SetTarget(context.Context, int64, bool, int64) error
	AcquireJob(context.Context, string, int64, string) (bool, error)
	SetPhase(context.Context, int64, string, string) error
	FinishJob(context.Context, string, int64, string, string, time.Duration) error
	RefreshTarget(context.Context, int64) error
	ScanTarget(context.Context, int64, bool) error
	ObserveCookie(context.Context, string, time.Duration) (time.Time, error)
	Publish(context.Context, *BorrowRoute, string) error
	Route(context.Context, int64, string, string) (*BorrowRoute, error)
	LookupSession(context.Context, int64, string) (string, bool, error)
	BindSession(context.Context, int64, string, string) (string, error)
	CandidateKnown(context.Context, int64, string, string) (bool, error)
	RejectCandidate(context.Context, int64, string, string, time.Time) error
	Cleanup(context.Context) error
	Revoke(context.Context, int64, string, string) error
	Event(context.Context, BorrowEvent) error
	Events(context.Context, int64, int64) ([]BorrowEvent, error)
	Now(context.Context) (time.Time, error)
}

type BorrowRotationAudit struct {
	Skipped            bool   `json:"skipped,omitempty"`
	SelectionKind      string `json:"selection_kind,omitempty"`
	JobID              string `json:"job_id"`
	Role               string `json:"role"`
	Trigger            string `json:"trigger"`
	AccountID          int64  `json:"account_id"`
	PreviousEntryID    int64  `json:"previous_entry_id"`
	EntryID            int64  `json:"entry_id"`
	EnvironmentVersion int64  `json:"environment_version"`
}
