package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type borrowStatusRepo struct{ normalBorrowTestRepo }

type borrowStatusAccounts struct {
	borrowTestAccounts
	resetAt      time.Time
	extra        map[string]any
	statusWrites int
	permanent    int
	temporary    int
	affectedID   int64
}

func (r *borrowStatusAccounts) SetRateLimited(ctx context.Context, _ int64, reset time.Time) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	r.resetAt = reset
	return nil
}
func (r *borrowStatusAccounts) UpdateExtra(_ context.Context, _ int64, extra map[string]any) error {
	r.extra = extra
	return nil
}
func (r *borrowStatusAccounts) SetError(_ context.Context, id int64, _ string) error {
	r.permanent++
	r.affectedID = id
	r.statusWrites++
	return nil
}
func (r *borrowStatusAccounts) SetTempUnschedulable(_ context.Context, id int64, _ time.Time, _ string) error {
	r.temporary++
	r.affectedID = id
	r.statusWrites++
	return nil
}

type borrowStatusBlocker struct {
	retryChecks int
	blocked     bool
}

func (r *borrowStatusBlocker) BlockAccountScheduling(*Account, time.Time, string) { r.blocked = true }
func (r *borrowStatusBlocker) ClearAccountSchedulingBlock(int64)                  {}
func (r *borrowStatusBlocker) ShouldRetryOpenAIOAuth429(*Account, http.Header, []byte) bool {
	r.retryChecks++
	return true
}

type borrowStatusUpstream struct {
	HTTPUpstream
	response       *http.Response
	calls          int
	beforeResponse func()
}

func (u *borrowStatusUpstream) DoWithTLS(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error) {
	u.calls++
	if u.beforeResponse != nil {
		u.beforeResponse()
	}
	return u.response, nil
}

func borrowSourceStatusFixture() (*TicketBorrowService, *borrowStatusRepo, *borrowStatusAccounts, *borrowStatusBlocker) {
	s, base := borrowFixture()
	a := s.owner.accountRepo.(borrowTestAccounts).account
	a.ID = 20
	r := &borrowStatusRepo{normalBorrowTestRepo: normalBorrowTestRepo{&borrowWorkerRepo{borrowTestRepo: base, source: boundBorrowTestSource(BorrowSource{ID: 2, AccountID: 20, Enabled: true, Model: "gpt-6-luna"})}}}
	accounts := &borrowStatusAccounts{borrowTestAccounts: borrowTestAccounts{account: a}}
	blocker := &borrowStatusBlocker{}
	s.repo, s.owner.accountRepo = r, accounts
	s.owner.openaiGatewayService = &OpenAIGatewayService{accountRepo: accounts, rateLimitService: &RateLimitService{accountRepo: accounts, cfg: &config.Config{}, runtimeBlocker: blocker}}
	return s, r, accounts, blocker
}

func TestBorrowSource401UsesNormalAuthClassification(t *testing.T) {
	for _, tc := range []struct {
		name, body             string
		refresh, cancel, stale bool
		permanent, temporary   int
	}{
		{"recoverable", `{"error":{"code":"invalid_token"}}`, true, false, false, 0, 1},
		{"revoked", `{"error":{"code":"token_revoked"}}`, true, false, false, 1, 0},
		{"permanent detail", `{"detail":"Unauthorized"}`, true, false, false, 1, 0},
		{"no refresh", `{}`, false, false, false, 1, 0},
		{"cancelled", `{"error":{"code":"token_revoked"}}`, true, true, false, 1, 0},
		{"IP first signal", `{"error":{"message":"Your IP is not authorized to make this request."}}`, true, false, false, 0, 0},
		{"refresh race", `{"error":{"code":"token_revoked"}}`, true, false, true, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, r, accounts, _ := borrowSourceStatusFixture()
			if tc.refresh {
				accounts.account.Credentials["refresh_token"] = "refresh-test"
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			up := &borrowStatusUpstream{response: &http.Response{StatusCode: 401, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}}
			up.beforeResponse = func() {
				if tc.cancel {
					cancel()
				}
				if tc.stale {
					copy := *accounts.account
					copy.Credentials = map[string]any{"access_token": "new-token", "refresh_token": "new-refresh"}
					accounts.account = &copy
				}
			}
			s.owner.httpUpstream = up
			_, _, err := s.acquireSource(borrowTestCandidateContext(ctx), r.source, BorrowSettings{})
			require.Error(t, err)
			require.Equal(t, 1, up.calls)
			require.Equal(t, tc.permanent, accounts.permanent)
			require.Equal(t, tc.temporary, accounts.temporary)
			require.Equal(t, "source_http_401", r.events[len(r.events)-1].Reason)
		})
	}
}

func TestBorrowSource429SynchronizesQuotaWithoutBusinessRetry(t *testing.T) {
	for _, tc := range []struct {
		name, used, body string
		resetSeconds     int
		cancel           bool
	}{
		{"exhausted long window", "100", `{}`, 30 * 86400, false},
		{"cancelled after exhausted headers", "100", `{}`, 30 * 86400, true},
		{"body only reset", "", `{"error":{"type":"usage_limit_reached","resets_in_seconds":3600}}`, 3600, false},
		{"non exhausted window uses short fallback", "35", `{}`, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, r, accounts, blocker := borrowSourceStatusFixture()
			headers := make(http.Header)
			if tc.used != "" {
				headers.Set("X-Codex-Primary-Used-Percent", tc.used)
				headers.Set("X-Codex-Primary-Window-Minutes", "43200")
				headers.Set("X-Codex-Primary-Reset-After-Seconds", "2592000")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			up := &borrowStatusUpstream{response: &http.Response{StatusCode: 429, Header: headers, Body: io.NopCloser(strings.NewReader(tc.body))}}
			if tc.cancel {
				up.beforeResponse = cancel
			}
			s.owner.httpUpstream = up
			before := time.Now()
			_, _, err := s.acquireSource(borrowTestCandidateContext(ctx), r.source, BorrowSettings{})
			require.Error(t, err)
			require.Equal(t, 1, up.calls)
			require.Zero(t, blocker.retryChecks)
			require.True(t, blocker.blocked)
			require.False(t, accounts.resetAt.IsZero())
			if tc.resetSeconds > 0 {
				require.WithinDuration(t, before.Add(time.Duration(tc.resetSeconds)*time.Second), accounts.resetAt, 2*time.Second)
			} else {
				require.Less(t, accounts.resetAt.Sub(before), time.Hour)
			}
			if tc.used != "" {
				require.NotEmpty(t, accounts.extra)
			}
		})
	}
}
