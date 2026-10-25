package service

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

type borrowRouteResponseObserver struct {
	owner *TicketBorrowService
	route *BorrowRoute
	lease string
	ctx   context.Context
	once  sync.Once
}

func (o *borrowRouteResponseObserver) finish(success bool, reason string, retry *time.Time) {
	if o == nil {
		return
	}
	o.once.Do(func() {
		repo, ok := o.owner.repo.(BorrowRoutePoolRepository)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(o.ctx), 2*time.Second)
		defer cancel()
		if err := repo.ObserveRouteOutcome(ctx, BorrowRouteOutcome{Route: o.route, Lease: o.lease, Success: success, Reason: reason, RetryAt: retry}); err != nil {
			slog.WarnContext(ctx, "borrow_route_outcome_failed", "route_id", o.route.ID, "error", err)
		}
		o.owner.notifyRoutePublished()
	})
}
func borrowRouteHTTPFailure(status int) string {
	if status == 429 {
		return "route_rate_limited"
	}
	if status >= 500 && status <= 599 {
		return "route_upstream_error"
	}
	return ""
}
func explicitBorrowRetryAt(header http.Header) *time.Time {
	raw := strings.TrimSpace(header.Get("Retry-After"))
	if raw == "" {
		return nil
	}
	// Malformed values do not manufacture an account-wide wait. The route has
	// its independent configured cooldown regardless of this optional extension.
	if _, err := http.ParseTime(raw); err != nil {
		for _, c := range raw {
			if c < '0' || c > '9' {
				return nil
			}
		}
	}
	at := time.Now().Add(borrowRetryAfter(raw, time.Now()))
	return &at
}
func (o *borrowRouteResponseObserver) headers(resp *http.Response) {
	if o == nil || resp == nil {
		return
	}
	if reason := borrowRouteHTTPFailure(resp.StatusCode); reason != "" {
		o.finish(false, reason, explicitBorrowRetryAt(resp.Header))
	}
	if resp.StatusCode >= 400 && borrowRouteHTTPFailure(resp.StatusCode) == "" {
		o.finish(false, "", nil)
	}
}

// Only explicit, structured terminal events qualify. A 200, DONE marker or
// output delta by itself never restores a route. User input errors stay local.
func borrowRouteSSEOutcome(data []byte) (terminal, success bool, reason string) {
	if !gjson.ValidBytes(data) {
		return false, false, ""
	}
	v := gjson.ParseBytes(data)
	typ := v.Get("type").String()
	status := v.Get("response.status").String()
	if (typ == "response.completed" || typ == "response.done") && v.Get("response").IsObject() && (status == "completed" || status == "") && v.Get("response.error").Type == gjson.Null {
		return true, true, ""
	}
	if typ == "response.cancelled" || typ == "response.canceled" {
		return true, false, ""
	}
	if typ != "error" && typ != "response.failed" && typ != "response.incomplete" {
		return false, false, ""
	}
	code := strings.ToLower(v.Get("error.code").String() + " " + v.Get("error.type").String() + " " + v.Get("response.error.code").String() + " " + v.Get("response.error.type").String() + " " + v.Get("code").String())
	if strings.Contains(code, "rate_limit") || strings.Contains(code, "too_many_requests") || v.Get("status").Int() == 429 || v.Get("error.status_code").Int() == 429 || v.Get("response.error.status_code").Int() == 429 || v.Get("status_code").Int() == 429 {
		return true, false, "route_rate_limited"
	}
	if strings.Contains(code, "server_error") || strings.Contains(code, "upstream_error") || strings.Contains(code, "overload") {
		return true, false, "route_upstream_error"
	}
	return true, false, ""
}
func (o *borrowRouteResponseObserver) event(data []byte) {
	if done, success, reason := borrowRouteSSEOutcome(data); done {
		o.finish(success, reason, nil)
	}
}

func (s *TicketBorrowService) startRouteHealth(ctx context.Context) {
	repo, ok := s.repo.(BorrowRoutePoolRepository)
	if !ok || s.owner == nil || s.owner.openaiGatewayService == nil {
		return
	}
	if err := repo.SweepRoutePool(ctx); err != nil {
		return
	}
	job, err := repo.AcquireRouteHealth(ctx, uuid.NewString())
	if err != nil {
		slog.WarnContext(ctx, "borrow_route_health_reservation_failed", "error", err)
		return
	}
	if job == nil {
		return
	}
	s.wg.Add(1)
	go func() { defer s.wg.Done(); s.runRouteHealth(ctx, repo, job) }()
}

func (s *TicketBorrowService) runRouteHealth(parent context.Context, repo BorrowRoutePoolRepository, job *BorrowRouteHealthJob) {
	// Pooled routes need one quality request, including cooldown recovery.
	// The extra time lets the owned job record its bounded request result.
	ctx, cancel := context.WithTimeout(parent, borrowRouteQualityTimeout+5*time.Second)
	defer cancel()
	if job.QualityConfig == nil {
		job.QualityConfig = DefaultBorrowRouteQualityConfig()
	}
	result := BorrowRouteOutcome{Route: job.Route}
	done := make(chan struct{})
	renewed := make(chan struct{})
	go func(leaseCtx context.Context) {
		defer close(renewed)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-leaseCtx.Done():
				return
			case <-ticker.C:
				check, stop := context.WithTimeout(leaseCtx, 3*time.Second)
				err := repo.RenewRouteHealth(check, job)
				stop()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}(ctx)
	defer func() {
		close(done)
		<-renewed
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if err := repo.FinishRouteHealth(cleanup, job, result); err != nil {
			slog.Warn("borrow_route_health_finish_failed", "route_id", job.Route.ID, "error", err)
		}
		s.notifyRoutePublished()
	}()
	gateway := s.owner.openaiGatewayService
	account, err := s.owner.accountRepo.GetByID(ctx, job.Route.TargetID)
	if err != nil || account == nil || !account.IsSchedulable() {
		return
	}
	token, _, err := gateway.GetAccessToken(ctx, account)
	if err != nil {
		return
	}
	account, err = s.owner.accountRepo.GetByID(ctx, job.Route.TargetID)
	if err != nil || account == nil {
		return
	}
	if s.targetIdentity(account) != job.Route.IdentityHash {
		_ = s.repo.Revoke(ctx, job.Route.TargetID, job.Route.ID, "target_identity_changed")
		return
	}
	payload := borrowRouteQualityBody(*job.QualityConfig)
	localReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/v1/responses", bytes.NewReader(payload))
	c := &gin.Context{Request: localReq}
	// Prepared skips recursive borrowed-route selection while preserving the
	// normal OpenAI transport pool and all lower-level presend hooks.
	ctx = context.WithValue(ctx, borrowPreparedKey{}, true)
	ctx = context.WithValue(ctx, borrowInternalKey{}, true)
	q := BorrowRequest{Kind: "probe", TargetID: job.Route.TargetID, RouteID: job.Route.ID, BatchID: job.Token, Revision: job.Route.TargetRevision, Identity: job.Route.IdentityHash, RouteHealth: job}
	ctx = context.WithValue(ctx, borrowProbeRequestKey{}, q)
	ctx = WithHTTPUpstreamRedirectsDisabled(ctx)
	req, err := gateway.buildUpstreamRequest(ctx, c, account, payload, token, true, uuid.NewString(), true)
	if err != nil {
		return
	}
	if req.URL.String() != job.Route.Secret.Endpoint {
		return
	}
	replaceBorrowCookie(req, &job.Route.Secret.Cookie)
	req = req.WithContext(context.WithValue(req.Context(), borrowBeforeSendKey{}, func(check context.Context) error {
		current, e := s.owner.accountRepo.GetByID(check, job.Route.TargetID)
		if e != nil {
			return e
		}
		if current == nil || !current.IsSchedulable() {
			return ErrBorrowUnavailable
		}
		return repo.CheckRouteHealth(check, job, s.targetIdentity(current))
	}))
	if err = BorrowBeforeSend(req); err != nil {
		return
	}
	profile, err := applyBorrowRouteFingerprint(req.Header, job.Route, job.QualityConfig.Model)
	if err != nil {
		result.Reason = "route_fingerprint_unavailable"
		return
	}
	upstream := &borrowRouteHealthUpstream{owner: s, request: q}
	s.checkRouteQuality(ctx, account, req, profile, upstream, job, &result)
}

func (s *TicketBorrowService) checkRouteQuality(ctx context.Context, account *Account, template *http.Request, profile *tlsfingerprint.Profile, upstream *borrowRouteHealthUpstream, job *BorrowRouteHealthJob, result *BorrowRouteOutcome) {
	qualityCtx, cancel := context.WithTimeout(withBorrowQualityRequest(template.Context()), borrowRouteQualityTimeout)
	defer cancel()
	req := borrowQualityRequest(template.WithContext(qualityCtx), *job.QualityConfig, uuid.NewString())
	quality, retry, err := probeBorrowRouteQuality(req, *job.QualityConfig, func(req *http.Request) (*http.Response, error) {
		return upstream.DoWithTLS(req, openAIAccountProxyURL(account), account.ID, account.Concurrency, profile)
	})
	// Local cancellation/ownership and budget rejection stay pending; they do
	// not become upstream failures or authorize admission.
	if ctx.Err() != nil || upstream.admissionStopped {
		return
	}
	if !quality.CookieDeadline.IsZero() {
		if _, e := s.repo.ObserveCookie(ctx, job.Route.CookieHash, time.Until(quality.CookieDeadline)); e != nil {
			return
		}
	}
	result.Quality, result.RetryAt = quality, retry
	result.Success = quality.Passed(*job.QualityConfig)
	result.Reason = quality.Reason
	if qualityCtx.Err() != nil {
		result.Success = false
		result.Reason = "route_quality_timeout"
		quality.Status, quality.Reason = "failed", result.Reason
	}
	if err != nil && result.Reason == "" {
		result.Reason = "route_quality_incomplete"
	}
}

// Reuse the candidate request tracker (deadlines, accounting, stream cleanup)
// with health ownership instead of inventing a candidate/acquisition lease.
type borrowRouteHealthUpstream struct {
	owner            *TicketBorrowService
	request          BorrowRequest
	admissionStopped bool
}

func (u *borrowRouteHealthUpstream) Do(req *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
	return u.DoWithTLS(req, proxy, id, concurrency, nil)
}
func (u *borrowRouteHealthUpstream) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	enteredTransport := false
	resp, err := u.owner.trackTargetRequest(req, u.request, proxy, id, concurrency, profile, func(req *http.Request, proxy string, id int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
		enteredTransport = true
		return u.owner.owner.httpUpstream.DoWithTLS(req, proxy, id, concurrency, profile)
	})
	if !enteredTransport || IsOpenAITurnAdmissionError(err) || errors.Is(err, ErrBorrowUnavailable) || errors.Is(err, ErrBorrowRequestBudget) || errors.Is(err, ErrBorrowProbeBusy) || errors.Is(err, ErrBorrowRotationPending) || errors.Is(err, ErrBorrowRecoveryWaiting) {
		u.admissionStopped = true
	}
	return resp, err
}
