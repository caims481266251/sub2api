package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"time"

	"github.com/google/uuid"
)

const (
	BorrowAcquisitionSelf               = "self"
	BorrowAcquisitionSource             = "source"
	DefaultBorrowAdmissionWindowSeconds = 80
	BorrowRollingInventory              = 2
)

var ErrBorrowAcquisitionIdentity = errors.New("borrow_acquisition_identity_invalid")
var ErrBorrowRotationRetired = errors.New("borrow_rotation_per_candidate")

func (t BorrowTarget) AcquisitionMode() string {
	if t.AcquisitionIdentity == BorrowAcquisitionSource {
		return BorrowAcquisitionSource
	}
	return BorrowAcquisitionSelf
}

func (s BorrowSettings) AdmissionSeconds() int {
	if s.AdmissionWindowSeconds != nil {
		return *s.AdmissionWindowSeconds
	}
	if s.TTLSeconds > 0 {
		remaining := 0
		if s.MinRouteRemainingSeconds != nil {
			remaining = *s.MinRouteRemainingSeconds
		}
		return max(1, s.TTLSeconds-remaining)
	}
	return DefaultBorrowAdmissionWindowSeconds
}

// All compatibility lives at the settings boundary. Internal admission uses W.
func ResolveBorrowAdmissionSettings(s *BorrowSettings, previous BorrowSettings) error {
	window := previous.AdmissionSeconds()
	if s.AdmissionWindowSeconds != nil {
		window = *s.AdmissionWindowSeconds
		if s.TTLSeconds != 0 && s.MinRouteRemainingSeconds != nil && s.TTLSeconds-*s.MinRouteRemainingSeconds != window {
			return errors.New("borrow_admission_window_invalid")
		}
	} else if s.TTLSeconds != 0 {
		if s.TTLSeconds < 30 || s.TTLSeconds > 240 {
			return errors.New("borrow_settings_invalid")
		}
		floor := previous.MinRouteRemainingSeconds
		if s.MinRouteRemainingSeconds != nil {
			floor = s.MinRouteRemainingSeconds
		}
		window = s.TTLSeconds
		if floor != nil {
			if *floor < 0 || *floor > 240 {
				return errors.New("borrow_settings_invalid")
			}
			window -= *floor
		}
	} else if s.MinRouteRemainingSeconds != nil {
		window = previous.TTLSeconds - *s.MinRouteRemainingSeconds
	}
	if window < 1 || window > 240 {
		return errors.New("borrow_admission_window_invalid")
	}
	s.AdmissionWindowSeconds = &window
	s.TTLSeconds = max(30, window)
	floor := s.TTLSeconds - window
	s.MinRouteRemainingSeconds = &floor
	lead := BorrowRenewalLead(window, 0)
	s.RenewBeforeSeconds = &lead
	return nil
}

// Candidate environments never become account configuration or business headers.
type BorrowCandidateEnvironment struct {
	ID                string
	EntryID           int64
	AllocationRound   int64
	Proxy             string
	SessionRandomized bool
	Fingerprint       *BorrowFingerprint
	StartedAt         time.Time
	AdmissionUntil    time.Time
}

// The -sid-XXXXXXXX-t- username format was verified with this pool's provider.
// Other formats remain unchanged; inventing proxy authentication is unsafe.
var borrowProxySessionPattern = regexp.MustCompile(`(-sid-)[A-Za-z0-9]{8}(-t-)`)

func RandomizeBorrowProxySession(raw string) (string, bool, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", false, errors.New("borrow_proxy_invalid")
	}
	if u.User == nil || len(borrowProxySessionPattern.FindAllStringIndex(u.User.Username(), -1)) != 1 {
		return raw, false, nil
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return "", false, err
	}
	username := borrowProxySessionPattern.ReplaceAllString(u.User.Username(), "${1}"+id.String()[:8]+"${2}")
	password, _ := u.User.Password()
	u.User = url.UserPassword(username, password)
	return u.String(), true, nil
}

type BorrowCandidateEnvironmentRepository interface {
	AllocateCandidateEnvironment(context.Context, string, string) (*BorrowCandidateEnvironment, error)
	StartCandidateEnvironment(context.Context, string) (time.Time, time.Time, error)
	MarkCandidateEnvironmentSent(context.Context, string) error
	CheckCandidateEnvironment(context.Context, string) error
	ReleaseCandidateEnvironment(context.Context, string) error
}

// BorrowCompatibilityRepository keeps a short-lived circuit breaker for a
// source/target/environment combination. It is optional so older stores and
// unit-test repositories continue to support normal borrowing.
type BorrowCompatibilityRepository interface {
	CheckBorrowCompatibility(context.Context, int64, string, string) (time.Time, bool, error)
	RecordBorrowCompatibility(context.Context, int64, string, string, string, bool) (time.Time, error)
}

type BorrowTargetIdentityRepository interface {
	SetTargetIdentity(context.Context, int64, bool, int64, int64, string) error
}

func (s *TicketBorrowService) SetTargetIdentity(ctx context.Context, id int64, enabled bool, revision, acquisitionRevision int64, identity string) error {
	if identity != BorrowAcquisitionSelf && identity != BorrowAcquisitionSource {
		return ErrBorrowAcquisitionIdentity
	}
	if err := s.validateTarget(ctx, id, enabled); err != nil {
		return err
	}
	r, ok := s.repo.(BorrowTargetIdentityRepository)
	if !ok {
		return ErrBorrowUnavailable
	}
	return r.SetTargetIdentity(ctx, id, enabled, revision, acquisitionRevision, identity)
}

// Require both upstream identifiers before merging account histories. Missing
// identity data must not merge unrelated accounts into the same bucket.
func borrowProxyIdentity(a *Account) string {
	account, user := borrowCredential(a.Credentials, "chatgpt_account_id"), borrowCredential(a.Credentials, "chatgpt_user_id")
	if account == "" || user == "" {
		return fmt.Sprintf("account:%d", a.ID)
	}
	raw, _ := json.Marshal([]string{account, user})
	return borrowHash(string(raw))
}

func borrowAdmissionDeadline(start time.Time, window int, expiry time.Time) time.Time {
	return minBorrowTime(start.Add(time.Duration(window)*time.Second), expiry)
}

// borrowCompatibilityKey scopes the circuit breaker to stable routing
// dimensions. Cookie pairs are deliberately excluded: a source can mint a
// fresh __oailb value for every candidate, and including it would make every
// failure look like a new combination so consecutive failures could never
// reach the backoff threshold. Cookie-pair hashes remain on the candidate as
// evidence for diagnosing which concrete ticket failed.
func borrowCompatibilityKey(targetIdentity string, sourceID int64, gateway string, entryID int64) string {
	return borrowHash(fmt.Sprintf("%s\x00%d\x00%s\x00%d", targetIdentity, sourceID, gateway, entryID))
}

// Search includes failed attempts. When search exceeds the admission window,
// replenish immediately while the current route still serves. No samples: 15s.
func BorrowRenewalLead(window int, observedMS int64) int {
	lead := 15
	if observedMS > 0 {
		lead = int((observedMS*3/2+999)/1000) + 2
	}
	return max(1, min(lead, max(1, window)))
}
