package service

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
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

// borrowCanonicalCookieHeader returns a stable, name-sorted representation of
// the two routing cookies. It is used only as input to a one-way hash; raw
// cookie values never enter borrow evidence.
func borrowCanonicalCookieHeader(header string) string {
	values := make(map[string]string, 2)
	for _, part := range strings.Split(header, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !ok || value == "" || (name != "__cflb" && name != "__oailb") {
			continue
		}
		values[name] = value
	}
	return borrowCanonicalCookieValues(values)
}

func borrowCanonicalCookieValues(values map[string]string) string {
	names := make([]string, 0, len(values))
	for name, value := range values {
		if value != "" && (name == "__cflb" || name == "__oailb") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+"="+values[name])
	}
	return strings.Join(parts, "; ")
}

// borrowCookieHeaderHashes returns balancer (__cflb), route (__oailb), and
// complete-pair hashes in that order. An empty component remains empty so
// callers can distinguish a missing Cookie from an actual value.
func borrowCookieHeaderHashes(header string) (balancer, route, pair string) {
	values := make(map[string]string, 2)
	for _, part := range strings.Split(header, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if ok && value != "" && (name == "__cflb" || name == "__oailb") {
			values[name] = value
		}
	}
	if values["__cflb"] != "" {
		balancer = borrowHash(values["__cflb"])
	}
	if values["__oailb"] != "" {
		route = borrowHash(values["__oailb"])
	}
	if canonical := borrowCanonicalCookieValues(values); canonical != "" {
		pair = borrowHash(canonical)
	}
	return balancer, route, pair
}

func borrowCookieResponseHashes(cookies []*http.Cookie) (balancer, route, pair string) {
	values := make(map[string]string, 2)
	for _, cookie := range cookies {
		if cookie == nil || cookie.Value == "" || (cookie.Name != "__cflb" && cookie.Name != "__oailb") || cookie.MaxAge < 0 {
			continue
		}
		values[cookie.Name] = cookie.Value
	}
	return borrowCookieHeaderHashes(borrowCanonicalCookieValues(values))
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
