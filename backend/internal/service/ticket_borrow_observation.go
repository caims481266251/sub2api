package service

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

var borrowVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+[a-zA-Z0-9.-]*$`)

func borrowEnvironment(req *http.Request, proxy string, profile *tlsfingerprint.Profile) BorrowEnvironment {
	// This fingerprint describes configured request identity, not an externally
	// observed IP or TLS fingerprint. Authorization and session tickets are omitted.
	headers := map[string]string{}
	for _, key := range []string{"User-Agent", "Version", "Originator", "Accept-Language", "X-Codex-Installation-ID", "X-Codex-Window-ID"} {
		headers[key] = req.Header.Get(key)
	}
	identity, _ := json.Marshal(headers)
	fingerprint, _ := json.Marshal(profile)
	out := BorrowEnvironment{IdentityHash: borrowHash(string(identity)), ProfileHash: borrowHash(string(fingerprint)), ProxyHash: borrowHash(proxy)}
	if version := req.Header.Get("Version"); len(version) <= 64 && borrowVersionPattern.MatchString(version) {
		out.ClientVersion = version
	}
	if req.GetBody != nil {
		if body, err := req.GetBody(); err == nil {
			data, readErr := io.ReadAll(io.LimitReader(body, (1<<20)+1))
			_ = body.Close()
			if readErr == nil && len(data) <= 1<<20 {
				out.BodyHash = borrowHash(string(data))
			}
		}
	}
	return out
}

func (d BorrowCandidateDetails) snapshot(expires time.Time, now time.Time) *BorrowCandidateDetails {
	// Copy before handing details to a repository: an acquisition's deferred event
	// and later candidate events must not share a mutable snapshot.
	if d.CookieObservedAt != nil {
		age := time.Since(*d.CookieObservedAt).Milliseconds()
		if age < 0 {
			age = 0
		}
		d.CookieAgeMS = &age
	}
	if !expires.IsZero() && !now.IsZero() {
		d.ExpiresAt = &expires
		remaining := expires.Sub(now).Milliseconds()
		d.RemainingMS = &remaining
	}
	return &d
}

func (s *TicketBorrowService) candidateEvent(ctx context.Context, event BorrowEvent, details *BorrowCandidateDetails, expires time.Time) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	now, _ := s.repo.Now(cleanup)
	event.Details = details.snapshot(expires, now)
	if err := s.repo.Event(cleanup, event); err != nil {
		// Losing telemetry must not change admission, but must be observable itself.
		slog.Warn("ticket_borrow_observation_failed", "target_id", event.TargetID, "source_id", event.SourceID, "reason", event.Reason)
	}
}
