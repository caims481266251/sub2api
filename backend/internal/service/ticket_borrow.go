package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/google/uuid"
)

type borrowInternalKey struct{}
type borrowPreparedKey struct{}
type borrowBeforeSendKey struct{}

// Rechecked for every transport attempt, including local pre-request retries.
func BorrowBeforeSend(req *http.Request) error {
	if req == nil {
		return nil
	}
	check, _ := req.Context().Value(borrowBeforeSendKey{}).(func(context.Context) error)
	if check != nil {
		return check(req.Context())
	}
	return nil
}

type TicketBorrowService struct {
	repo             TicketBorrowRepository
	owner            *AccountTestService
	oauth            *OpenAIOAuthService
	cancel           context.CancelFunc
	wg               sync.WaitGroup
	nextCleanup      time.Time
	nextEventCleanup time.Time
}

func NewTicketBorrowService(repo TicketBorrowRepository, owner *AccountTestService, oauth *OpenAIOAuthService) *TicketBorrowService {
	return &TicketBorrowService{repo: repo, owner: owner, oauth: oauth}
}

// Explicit administrator operation; never runs as part of startup/import.
func (s *TicketBorrowService) TransferSource(ctx context.Context, id int64) (*BorrowSource, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	a, err := s.owner.accountRepo.GetByID(ctx, id)
	if err != nil || a == nil {
		return nil, ErrBorrowConflict
	}
	if !a.IsOpenAIOAuthLike() || a.ParentAccountID != nil {
		return nil, errors.New("borrow_target_unsupported")
	}
	legacy := s.owner.cfg.AstraRouting(ctx)
	if legacy.CookiePool.Enabled {
		for _, old := range append(append([]int64{}, legacy.CookiePool.SourceAccountIDs...), legacy.CookiePool.TargetAccountIDs...) {
			if old == id {
				return nil, errors.New("borrow_legacy_config_conflict")
			}
		}
	}
	provider := s.owner.openaiGatewayService.openAITokenProvider
	if provider == nil || provider.tokenCache == nil || provider.refreshAPI == nil {
		return nil, ErrBorrowConflict
	}
	key := OpenAITokenCacheKey(a)
	local := provider.refreshAPI.getLocalLock(key)
	if err = local.Lock(ctx); err != nil {
		return nil, ErrBorrowConflict
	}
	defer local.Unlock()
	locked, err := provider.tokenCache.AcquireRefreshLock(ctx, key, 60*time.Second)
	if err != nil || !locked {
		return nil, ErrBorrowConflict
	}
	defer func() {
		release, stop := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer stop()
		_ = provider.tokenCache.ReleaseRefreshLock(release, key)
	}()
	a, err = s.owner.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if borrowCredential(a.Credentials, "chatgpt_account_id") == "" {
		return nil, errors.New("borrow_source_credentials_required")
	}
	source := &BorrowSource{Name: a.Name, Model: "gpt-6-luna", Enabled: true, Secret: BorrowSourceSecret{Credentials: a.Credentials, DeviceID: uuid.NewString()}}
	if model := strings.TrimSpace(legacy.CookiePool.SourceAccountModels[id]); model != "" {
		source.Model = model
	}
	if proxy := openAIAccountProxyURL(a); proxy != "" {
		source.Secret.Proxies = []string{proxy}
	}
	source.SubjectHash = borrowHash(borrowCredential(a.Credentials, "chatgpt_account_id") + "\x00" + borrowCredential(a.Credentials, "chatgpt_user_id"))
	if err = s.repo.TransferSource(ctx, a, source); err != nil {
		return nil, err
	}
	_ = provider.tokenCache.DeleteAccessToken(ctx, key)
	return source, nil
}
func BorrowTransportPrepared(ctx context.Context) bool {
	v, _ := ctx.Value(borrowPreparedKey{}).(bool)
	return v
}
func borrowInternal(ctx context.Context) bool {
	v, _ := ctx.Value(borrowInternalKey{}).(bool)
	return v
}
func borrowHash(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
func borrowCredential(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return strings.TrimSpace(s)
}
func borrowAccountIdentity(a *Account) string {
	data, _ := json.Marshal([]any{a.ID, a.Credentials["chatgpt_account_id"], a.Credentials["chatgpt_user_id"], openAIAccountProxyURL(a), a.IsTLSFingerprintEnabled(), a.GetTLSFingerprintProfileID()})
	return borrowHash(string(data))
}
func (s *TicketBorrowService) Sources(ctx context.Context) ([]BorrowSource, error) {
	sources, err := s.repo.Sources(ctx)
	if err != nil {
		return nil, err
	}
	for i := range sources {
		if !sources[i].Enabled {
			continue
		}
		if _, e := s.normalSourceAccount(ctx, sources[i]); e != nil {
			sources[i].Enabled, sources[i].UnavailableReason = false, borrowHealthReason(e)
		}
	}
	return sources, nil
}
func (s *TicketBorrowService) SaveSource(ctx context.Context, id int64, input BorrowSourceInput) (*BorrowSource, error) {
	if len(strings.TrimSpace(input.Name)) == 0 || len(input.Name) > 100 {
		return nil, errors.New("borrow_source_name_invalid")
	}
	if input.Model == "" {
		input.Model = "gpt-6-luna"
	}
	if len(input.Model) > 256 || strings.ContainsAny(input.Model, " \t\r\n*") {
		return nil, errors.New("borrow_source_model_invalid")
	}
	source := &BorrowSource{ID: id, Secret: BorrowSourceSecret{DeviceID: uuid.NewString()}}
	if id != 0 {
		old, err := s.repo.Source(ctx, id)
		if err != nil {
			return nil, err
		}
		source = old
		if input.Revision != source.Revision {
			return nil, ErrBorrowConflict
		}
	}
	if len(input.Credentials) > 0 {
		source.Secret.Credentials = input.Credentials
	}
	c := source.Secret.Credentials
	if borrowCredential(c, "chatgpt_account_id") == "" || (borrowCredential(c, "access_token") == "" && borrowCredential(c, "refresh_token") == "") {
		return nil, errors.New("borrow_source_credentials_required")
	}
	if input.Proxies != nil {
		if len(*input.Proxies) > 100 {
			return nil, errors.New("borrow_proxy_limit")
		}
		source.Secret.Proxies = nil
		for _, raw := range *input.Proxies {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			if !strings.Contains(raw, "://") {
				parts := strings.SplitN(raw, ":", 4)
				if len(parts) != 4 {
					return nil, errors.New("borrow_proxy_invalid")
				}
				u := url.URL{Scheme: "http", Host: parts[0] + ":" + parts[1], User: url.UserPassword(parts[2], parts[3])}
				raw = u.String()
			}
			u, err := url.Parse(raw)
			if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5") || u.RawQuery != "" || u.Fragment != "" {
				return nil, errors.New("borrow_proxy_invalid")
			}
			source.Secret.Proxies = append(source.Secret.Proxies, raw)
		}
	}
	source.Name = strings.TrimSpace(input.Name)
	source.Model = input.Model
	source.Enabled = input.Enabled
	source.RotateIdentity = input.RotateIdentity
	source.SubjectHash = borrowHash(borrowCredential(c, "chatgpt_account_id") + "\x00" + borrowCredential(c, "chatgpt_user_id"))
	if source.Secret.Profile == nil {
		source.Secret.Profile = borrowSourceProfile(source.Secret.DeviceID)
	}
	if err := s.repo.SaveSource(ctx, source); err != nil {
		return nil, err
	}
	source.ProxyCount = len(source.Secret.Proxies)
	return source, nil
}
func (s *TicketBorrowService) DeleteSource(ctx context.Context, id, revision int64) error {
	return s.repo.DeleteSource(ctx, id, revision)
}
func (s *TicketBorrowService) Settings(ctx context.Context) (BorrowSettings, error) {
	return s.repo.Settings(ctx)
}
func (s *TicketBorrowService) SaveSettings(ctx context.Context, v BorrowSettings) error {
	NormalizeBorrowSettings(&v)
	if err := v.Replenishment.Validate(); err != nil {
		return err
	}
	if v.ContinueTimeoutSeconds != nil && (*v.ContinueTimeoutSeconds < 1 || *v.ContinueTimeoutSeconds > 45) {
		return errors.New("borrow_settings_invalid")
	}
	if v.MinRouteRemainingSeconds != nil && (*v.MinRouteRemainingSeconds < 0 || *v.MinRouteRemainingSeconds > 240) {
		return errors.New("borrow_settings_invalid")
	}
	if (v.ProbeConcurrency != nil && (*v.ProbeConcurrency < 1 || *v.ProbeConcurrency > 32)) ||
		(v.RenewBeforeSeconds != nil && (*v.RenewBeforeSeconds < 0 || *v.RenewBeforeSeconds > 240)) {
		return ErrBorrowProbePolicyInvalid
	}
	if v.TTLSeconds < 30 || v.TTLSeconds > 240 || v.Recovery.ProbeRequestsPerMinute < 2 || v.Recovery.ProbeRequestsPerMinute > 60 || v.Recovery.SourceRequestsPerMinute < 1 || v.Recovery.SourceRequestsPerMinute > 120 {
		return errors.New("borrow_settings_invalid")
	}
	return s.repo.SaveSettings(ctx, v)
}
func (s *TicketBorrowService) Events(ctx context.Context, before, target int64) ([]BorrowEvent, error) {
	return s.repo.Events(ctx, before, target)
}
func (s *TicketBorrowService) Scan(ctx context.Context, id int64, cancel bool) error {
	return s.repo.ScanTarget(ctx, id, cancel)
}
func (s *TicketBorrowService) Refresh(ctx context.Context, id int64) error {
	return s.repo.RefreshTarget(ctx, id)
}
func (s *TicketBorrowService) SetTarget(ctx context.Context, id int64, enabled bool, revision int64) error {
	if enabled {
		a, err := s.owner.accountRepo.GetByID(ctx, id)
		if err != nil {
			return err
		}
		if a == nil || !a.IsOpenAIOAuthLike() || a.ParentAccountID != nil || openAICodexStateProbeUnsupportedReason(a, "gpt-6-astra", false) != "" {
			return errors.New("borrow_target_unsupported")
		}
		old := s.owner.cfg.AstraRouting(ctx)
		for _, other := range append(append([]int64{}, old.CookiePool.SourceAccountIDs...), old.CookiePool.TargetAccountIDs...) {
			if old.CookiePool.Enabled && other == id {
				return errors.New("borrow_legacy_config_conflict")
			}
		}
	}
	return s.repo.SetTarget(ctx, id, enabled, revision)
}
func (s *TicketBorrowService) Status(ctx context.Context, id int64) (BorrowTarget, error) {
	target, err := s.repo.Target(ctx, id)
	if err != nil || !target.Enabled {
		return target, err
	}
	a, err := s.owner.accountRepo.GetByID(ctx, target.AccountID)
	if err != nil {
		return target, err
	}
	if a == nil || !a.IsSchedulable() {
		target.Reason = "target_unavailable"
		return target, nil
	}
	_, identity, err := s.targetEnvironment(ctx, a)
	if err != nil {
		if errors.Is(err, ErrBorrowRotationPending) || errors.Is(err, ErrBorrowUnavailable) {
			target.Reason = "target_proxy_pool_unavailable"
			if errors.Is(err, ErrBorrowRotationPending) {
				target.Reason = "borrow_rotation_pending"
			}
			return target, nil
		}
		return target, err
	}
	if r, ok := s.repo.(BorrowReplenishmentRepository); ok {
		target.Replenishment, err = r.Replenishment(ctx, target.AccountID, identity)
		if err != nil {
			return target, err
		}
	}
	if allocator, ok := s.repo.(BorrowRouteLeaseRepository); ok {
		target.BusinessRoutes, err = allocator.RouteStatuses(ctx, target.AccountID, identity)
		if err != nil {
			return target, err
		}
		for _, route := range target.BusinessRoutes {
			target.BusinessInflight += route.Inflight
		}
	}
	if r, ok := s.repo.(BorrowInventoryRepository); ok {
		target.BorrowInventory, err = r.Inventory(ctx, target.AccountID, identity)
		if err != nil {
			return target, err
		}
	}
	target.BusinessConcurrency = a.Concurrency
	route, err := s.repo.Route(ctx, target.AccountID, identity, "")
	if errors.Is(err, ErrBorrowUnavailable) {
		return target, nil
	}
	if err != nil {
		return target, err
	}
	target.Ready = true
	target.Gateway = route.Gateway
	target.ExpiresAt = &route.ExpiresAt
	if !route.UsableUntil.IsZero() {
		target.UsableUntil = &route.UsableUntil
	}
	return target, nil
}
func (s *TicketBorrowService) Targets(ctx context.Context) ([]BorrowTarget, error) {
	targets, err := s.repo.Targets(ctx)
	if err != nil {
		return nil, err
	}
	for i := range targets {
		t, e := s.Status(ctx, targets[i].AccountID)
		if e != nil {
			return nil, e
		}
		targets[i] = t
	}
	return targets, nil
}
func (s *TicketBorrowService) Enabled(ctx context.Context, id int64) (bool, error) {
	if id <= 0 || borrowInternal(ctx) {
		return false, nil
	}
	t, e := s.repo.Target(ctx, id)
	return t.Enabled, e
}

// Eligibility never mutates normal priority, weight, schedulable, or cooldown.
func (s *TicketBorrowService) Check(ctx context.Context, id int64, model, transport string) error {
	if !borrowInternal(ctx) && !borrowHTTPContinuationAllowsAccount(ctx, id) {
		return ErrBorrowUnavailable
	}
	if borrowInternal(ctx) || id <= 0 {
		return nil
	}
	continuation := borrowHTTPContinuationFromContext(ctx)
	t, err := s.repo.Target(ctx, id)
	if err != nil {
		return ErrBorrowUnavailable
	}
	if !t.Enabled {
		if continuation != nil || borrowPreferredPass(ctx) {
			return ErrBorrowUnavailable
		}
		return nil
	}
	if model != "gpt-6-astra" || transport != "http" {
		return ErrBorrowUnavailable
	}
	a, err := s.owner.accountRepo.GetByID(ctx, t.AccountID)
	if err != nil || a == nil || !a.IsSchedulable() {
		return ErrBorrowUnavailable
	}
	_, identity, err := s.targetEnvironment(ctx, a)
	if err != nil {
		return ErrBorrowUnavailable
	}
	if continuation != nil {
		repo, ok := s.repo.(BorrowHTTPContinuationRepository)
		if !ok {
			return ErrBorrowUnavailable
		}
		return repo.CheckHTTPContinuation(ctx, id, identity, continuation.Response, continuation.Session)
	}
	hint, _ := ctx.Value(borrowBindingKey{}).(borrowBindingHint)
	if repo, ok := s.repo.(BorrowLatestRouteRepository); ok && hint.Response == "" {
		return repo.CheckLatestRoute(ctx, id, identity, hint.Session)
	}
	preferred, err := s.borrowPreferredRoute(ctx, t.AccountID, hint)
	if err != nil {
		return err
	}
	_, err = s.repo.Route(ctx, t.AccountID, identity, preferred)
	if err != nil {
		return ErrBorrowUnavailable
	}
	return nil
}

// This hook is below both legacy borrowing and business forwarding. Only the
// dedicated worker's unexported context can bypass admission.
func (s *TicketBorrowService) ForwardHTTP(req *http.Request, proxy string, id int64, n int, profile *tlsfingerprint.Profile, send func(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error)) (*http.Response, error) {
	prepared := func(r *http.Request) *http.Request {
		return r.WithContext(context.WithValue(r.Context(), borrowPreparedKey{}, true))
	}
	if req == nil {
		return nil, ErrBorrowUnavailable
	}
	req = req.WithContext(BorrowBusinessRecoveryContext(req.Context()))
	if !borrowInternal(req.Context()) && !borrowHTTPContinuationAllowsAccount(req.Context(), id) {
		return nil, ErrBorrowUnavailable
	}
	if borrowInternal(req.Context()) || id <= 0 {
		if q, ok := req.Context().Value(borrowProbeRequestKey{}).(BorrowRequest); ok {
			return s.trackTargetRequest(prepared(req), q, proxy, id, n, profile, send)
		}
		if borrowInternal(req.Context()) {
			req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileBorrowProbe))
		}
		return send(prepared(req), proxy, id, n, profile)
	}
	continuation := borrowHTTPContinuationFromContext(req.Context())
	target, err := s.repo.Target(req.Context(), id)
	if err != nil {
		return nil, ErrBorrowUnavailable
	}
	if !target.Enabled {
		if continuation != nil {
			return nil, ErrBorrowUnavailable
		}
		return send(prepared(req), proxy, id, n, profile)
	}
	// Unsupported operations may not silently fall back to a direct request.
	if req.URL == nil || req.URL.RawQuery != "" || req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/responses") || req.GetBody == nil {
		return nil, ErrBorrowUnavailable
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, ErrBorrowUnavailable
	}
	var payload struct {
		Model              string `json:"model"`
		PreviousResponseID string `json:"previous_response_id"`
	}
	err = json.NewDecoder(io.LimitReader(body, 32<<20)).Decode(&payload)
	body.Close()
	if err != nil || payload.Model != "gpt-6-astra" {
		return nil, ErrBorrowUnavailable
	}
	account, err := s.owner.accountRepo.GetByID(req.Context(), target.AccountID)
	if err != nil || account == nil || !account.IsSchedulable() {
		return nil, ErrBorrowUnavailable
	}
	_, identity, err := s.targetEnvironment(req.Context(), account)
	if err != nil {
		return nil, ErrBorrowUnavailable
	}
	hint, hasHint := req.Context().Value(borrowBindingKey{}).(borrowBindingHint)
	if !hasHint {
		hint = borrowBindingHint{borrowSessionKey(req.Header), borrowResponseKey(payload.PreviousResponseID)}
	}
	if payload.PreviousResponseID != "" {
		response := borrowResponseKey(payload.PreviousResponseID)
		if continuation != nil && continuation.Response != response {
			return nil, ErrBorrowUnavailable
		}
		if hint.Response != "" && hint.Response != response {
			return nil, ErrBorrowUnavailable
		}
		hint.Response = response
	}
	allocator, ok := s.repo.(BorrowRouteLeaseRepository)
	if !ok {
		return nil, ErrBorrowUnavailable
	}
	leaseID := uuid.NewString()
	var route *BorrowRoute
	var choice *BorrowContinuationRoute
	preferred := ""
	if continuation != nil {
		repo, ok := s.repo.(BorrowHTTPContinuationRepository)
		if !ok {
			return nil, ErrBorrowUnavailable
		}
		choice, err = repo.ReserveHTTPContinuation(req.Context(), id, identity, continuation.Response, continuation.Session, continuation.FreshSession, leaseID)
		if err == nil {
			route = choice.Route
		}
	} else if repo, ok := s.repo.(BorrowLatestRouteRepository); ok && hint.Response == "" {
		choice, err = repo.ReserveLatestRoute(req.Context(), id, identity, hint.Session, uuid.NewString(), leaseID)
		if err == nil {
			route = choice.Route
		}
	} else {
		preferred, err = s.borrowPreferredRoute(req.Context(), target.AccountID, hint)
		if err == nil {
			route, err = allocator.ReserveRoute(req.Context(), target.AccountID, identity, preferred, hint.Session, leaseID)
		}
	}
	if err != nil {
		return nil, ErrBorrowUnavailable
	}
	use := newBorrowRouteUse(req.Context(), allocator, leaseID, 15*time.Second)
	bodyOwnsLease := false
	defer func() {
		if !bodyOwnsLease {
			use.finish()
		}
	}()
	req = req.WithContext(use.ctx)
	if err := BorrowBusinessRecoveryFromContext(req.Context()).Activate(target.AccountID, continuation != nil || preferred != "" || payload.PreviousResponseID != "" || req.Header.Get("X-Codex-Turn-State") != ""); err != nil {
		return nil, err
	}
	if route.Secret.Endpoint == "" || req.URL.Scheme+"://"+req.URL.Host+req.URL.EscapedPath() != route.Secret.Endpoint {
		return nil, ErrBorrowUnavailable
	}
	r := req.Clone(context.WithValue(req.Context(), borrowPreparedKey{}, true))
	r = r.WithContext(WithHTTPUpstreamProfile(r.Context(), HTTPUpstreamProfileOpenAIHarvest))
	r = r.WithContext(context.WithValue(r.Context(), borrowBeforeSendKey{}, func(ctx context.Context) error {
		latest, e := s.owner.accountRepo.GetByID(ctx, target.AccountID)
		if e != nil || latest == nil || !latest.IsSchedulable() {
			return ErrBorrowUnavailable
		}
		_, identity, e := s.targetEnvironment(ctx, latest)
		if e != nil {
			return ErrBorrowUnavailable
		}
		if e := allocator.RenewRoute(ctx, leaseID); e != nil {
			return ErrBorrowUnavailable
		}
		_, e = s.repo.Route(ctx, target.AccountID, identity, route.ID)
		if e != nil {
			return ErrBorrowUnavailable
		}
		return nil
	}))
	// Match the verified environment, while preserving per-conversation state.
	for _, key := range []string{"Authorization", "ChatGPT-Account-ID", "User-Agent", "Version", "Originator", "Accept-Language", "X-Codex-Installation-ID", "X-Codex-Window-ID", "Cookie"} {
		r.Header.Del(key)
	}
	for k, v := range route.Secret.Headers {
		r.Header[k] = append([]string{}, v...)
	}
	if choice != nil {
		if err := applyBorrowHTTPContinuation(r, choice); err != nil {
			return nil, ErrBorrowUnavailable
		}
		slog.InfoContext(r.Context(), "borrow latest business route reserved", "target_id", id, "route_id", route.ID, "session_restarted", choice.Restarted, "effective_session_hash", borrowSessionKey(r.Header))
	}
	// Routing data is borrowed; all authorization remains the target's own.
	replaceBorrowCookie(r, &route.Secret.Cookie)
	r = r.WithContext(WithHTTPUpstreamRedirectsDisabled(r.Context()))
	// Business always uses the target account exit; a borrowed route only supplies routing data.
	resp, err := send(r, openAIAccountProxyURL(account), id, n, route.Secret.Profile)
	if resp != nil {
		for _, c := range resp.Cookies() {
			if c.Name == "__oailb" && c.Value == route.Secret.Cookie.Value && c.MaxAge >= 0 {
				ttl := time.Until(route.ExpiresAt)
				if c.MaxAge > 0 {
					ttl = min(ttl, time.Duration(c.MaxAge)*time.Second)
				} else if !c.Expires.IsZero() {
					ttl = min(ttl, time.Until(c.Expires))
				}
				_, _ = s.repo.ObserveCookie(req.Context(), route.CookieHash, ttl)
			}
		}
		reason := borrowResponseFailure(r, route, resp)
		if reason != "" {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(req.Context()), 3*time.Second)
			_ = s.repo.Revoke(ctx, target.AccountID, route.ID, reason)
			_ = s.repo.RefreshTarget(ctx, target.AccountID)
			_ = s.repo.Event(ctx, BorrowEvent{TargetID: target.AccountID, SourceID: route.SourceID, RouteID: route.ID, Stage: "route", Reason: reason})
			cancel()
			if resp.Body != nil {
				resp.Body.Close()
			}
			return nil, ErrBorrowUnavailable
		}
		if err == nil && resp.Body != nil && resp.Body != http.NoBody {
			observed := &borrowObservedBody{ReadCloser: resp.Body, recoveryContext: req.Context(), owner: s, target: target.AccountID, source: route.SourceID, route: route.ID}
			if choice != nil {
				observed.effectiveSession = choice.SessionID
				observed.httpContinuation = true
			}
			use.attach(observed)
			resp.Body = &borrowLeasedBody{ReadCloser: observed, use: use}
			bodyOwnsLease = true
		} else if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}
	return resp, err
}
func replaceBorrowCookie(req *http.Request, c *http.Cookie) {
	cookies := req.Cookies()
	req.Header.Del("Cookie")
	for _, old := range cookies {
		if old.Name != "__oailb" {
			req.AddCookie(old)
		}
	}
	req.AddCookie(&http.Cookie{Name: "__oailb", Value: c.Value})
}
func borrowResponseFailure(req *http.Request, route *BorrowRoute, resp *http.Response) string {
	before := req.Header.Get("X-Codex-Turn-State")
	after := extractOpenAICodexTurnState(resp.Header)
	if before != "" && after != "" && after != before {
		return "target_ticket_changed"
	}
	for _, c := range resp.Cookies() {
		if c.Name == "__oailb" && (c.Value != route.Secret.Cookie.Value || c.MaxAge < 0 || (!c.Expires.IsZero() && !time.Now().Before(c.Expires))) {
			return "target_route_changed"
		}
	}
	if resp.StatusCode == 401 {
		return "target_unauthorized"
	}
	return ""
}

var borrowGatewayPattern = regexp.MustCompile(`^chat\.gateway\.unified-[0-9]+\.api\.openai\.com$`)

func borrowGateway(cookie string) string {
	parts := strings.Split(cookie, ".")
	if len(parts) != 3 {
		return ""
	}
	b, e := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil {
		return ""
	}
	var c struct {
		Host string `json:"host"`
	}
	if json.Unmarshal(b, &c) != nil || !borrowGatewayPattern.MatchString(c.Host) {
		return ""
	}
	return c.Host
}
func (s *TicketBorrowService) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(time.Second)
		nextBorrow := time.Time{}
		defer ticker.Stop()
		for {
			if !time.Now().Before(nextBorrow) {
				s.tick(ctx)
				nextBorrow = time.Now().Add(time.Second)
			} else {
				s.processRotations(ctx)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
func (s *TicketBorrowService) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}
func (s *TicketBorrowService) processRotations(ctx context.Context) bool {
	if r, ok := s.repo.(BorrowRotationRepository); ok {
		if err := r.ProcessRotations(ctx); err != nil {
			if ctx.Err() == nil {
				slog.Warn("ticket_borrow_rotation_processing_failed")
			}
			return false
		}
	}
	return true
}
func (s *TicketBorrowService) tick(ctx context.Context) {
	s.cleanupEvents(ctx)
	if !s.processRotations(ctx) {
		return
	}
	if time.Now().After(s.nextCleanup) {
		_ = s.repo.Cleanup(ctx)
		s.nextCleanup = time.Now().Add(time.Hour)
	}
	settings, err := s.repo.Settings(ctx)
	if err != nil || !settings.Running {
		return
	}
	targets, err := s.repo.Targets(ctx)
	if err != nil {
		return
	}
	for _, t := range targets {
		if ctx.Err() != nil {
			return
		}
		status, err := s.Status(ctx, t.AccountID)
		if err != nil {
			continue
		}
		if !borrowTargetNeedsProbe(status) {
			continue
		}
		a, err := s.owner.accountRepo.GetByID(ctx, t.AccountID)
		if err != nil || a == nil {
			continue
		}
		_, identity, err := s.targetEnvironment(ctx, a)
		if err != nil {
			continue
		}
		jobCtx := WithBorrowAcquisition(ctx, BorrowAcquisition{TargetID: t.AccountID, Revision: t.Revision, Identity: identity})
		token := uuid.NewString()
		ok, err := s.repo.AcquireJob(jobCtx, "target", t.AccountID, token)
		if err != nil || !ok {
			continue
		}
		s.wg.Add(1)
		go func(t BorrowTarget) { defer s.wg.Done(); s.runTarget(ctx, t, settings, token) }(t)
	}
}
func (s *TicketBorrowService) runTarget(parent context.Context, target BorrowTarget, settings BorrowSettings, lease string) {
	ctx, cancel := context.WithTimeout(context.WithValue(parent, borrowInternalKey{}, true), 150*time.Second)
	defer cancel()
	reason := "no_candidate"
	delay := time.Duration(0)
	scanProcessed := 0
	diagnostic := target.ScanOffset < len(target.ScanSources)
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
		defer stop()
		if diagnostic && scanProcessed > 0 {
			_ = s.repo.AdvanceScan(cleanup, target.AccountID, lease, scanProcessed)
		}
		_ = s.repo.FinishJob(cleanup, "target", target.AccountID, lease, reason, delay)
	}()
	sources, err := s.repo.Sources(ctx)
	if err != nil {
		reason = "source_store_unavailable"
		return
	}
	enabledSources := sources[:0]
	for _, source := range sources {
		if source.UnavailableReason == "source_proxy_pool_unavailable" {
			reason = "source_proxy_pool_unavailable"
		}
		if source.Enabled && source.AccountID > 0 && source.UseProxyPool && source.PoolEntryID > 0 &&
			!source.NextAttemptAt.After(time.Now()) && (source.LeaseUntil == nil || !source.LeaseUntil.After(time.Now())) {
			if _, e := s.normalSourceAccount(ctx, source); e == nil {
				enabledSources = append(enabledSources, source)
			}
		}
	}
	sources = enabledSources
	if len(sources) == 0 {
		delay = 5 * time.Second
		return
	}
	account, err := s.normalBorrowAccount(ctx, target.AccountID, OpenAICodexStateProbeDefaultModel)
	if err != nil {
		reason = "target_unavailable"
		return
	}
	template, proxy, _, err := s.owner.astraTargetTemplate(ctx, target.AccountID)
	if err != nil {
		reason = "target_credentials_unavailable"
		return
	}
	// Refresh may have rotated credentials; publish only the identity actually used.
	account, err = s.owner.accountRepo.GetByID(ctx, target.AccountID)
	if err != nil {
		return
	}
	environment, identity, err := s.targetEnvironment(ctx, account)
	if err != nil {
		reason = "target_proxy_pool_unavailable"
		if errors.Is(err, ErrBorrowRotationPending) {
			reason = "borrow_rotation_pending"
		}
		return
	}
	acquisition := BorrowAcquisition{TargetID: target.AccountID, Revision: target.Revision, Lease: lease, Identity: identity}
	ctx = WithBorrowAcquisition(ctx, acquisition)
	if e := s.repo.CheckAcquisition(ctx, acquisition); e != nil {
		reason = borrowAcquisitionReason(e)
		return
	}
	var replenishment *BorrowReplenishmentState
	if r, ok := s.repo.(BorrowReplenishmentRepository); ok && !diagnostic {
		replenishment, err = r.Replenishment(ctx, target.AccountID, identity)
		if err != nil {
			reason = "source_store_unavailable"
			return
		}
	}
	var profile *tlsfingerprint.Profile
	if s.owner.tlsFPProfileService != nil {
		profile = s.owner.tlsFPProfileService.ResolveTLSProfile(account)
	}
	if environment.EntryID > 0 {
		proxy = environment.Proxy
		template.Header.Set("X-Codex-Installation-ID", environment.DeviceID)
		template.Header.Set("X-Codex-Window-ID", environment.DeviceID)
		profile = borrowSourceProfile(environment.DeviceID)
	}
	// Fence probe dispatch after connection waits and before retries/publication.
	expectedIdentity := identity
	template = template.WithContext(context.WithValue(template.Context(), borrowBeforeSendKey{}, func(check context.Context) error {
		latest, e := s.normalBorrowAccount(check, target.AccountID, OpenAICodexStateProbeDefaultModel)
		if e != nil || latest == nil {
			return ErrBorrowUnavailable
		}
		_, current, e := s.targetEnvironment(check, latest)
		if e != nil || current != expectedIdentity || borrowCredential(latest.Credentials, "access_token") != borrowCredential(account.Credentials, "access_token") {
			return ErrBorrowUnavailable
		}
		return nil
	}))
	targetModel := normalizeOpenAIModelForUpstream(account, account.GetMappedModel(OpenAICodexStateProbeDefaultModel))
	policy := target.ProbePolicy.effective()
	type candidate struct {
		source  int64
		cookie  *http.Cookie
		expires time.Time
		details *BorrowCandidateDetails
		routeID string
	}
	targetEnvironment := borrowEnvironment(template, proxy, profile)
	targetEnvironment.BodyHash = borrowHash(string(openAICodexStateProbeBody(targetModel)))
	emitCandidate := func(c candidate, stage, outcome string) {
		c.details.Target = targetEnvironment
		c.details.TargetIdentityHash = identity
		c.details.TargetRevision = target.Revision
		s.candidateEvent(ctx, BorrowEvent{TargetID: target.AccountID, SourceID: c.source, RouteID: c.routeID, Stage: stage, Reason: outcome}, c.details, c.expires)
	}
	candidates := make(chan candidate, policy.Concurrency)
	var acquiredCount int
	var acquiredMu sync.Mutex
	var acquiring sync.WaitGroup
	// Diagnostic runs one source at a time so a cooldown/pause cannot silently
	// advance past unverified candidates from an already acquired batch.

	batch := make([]BorrowSource, 0, policy.Concurrency)
	if diagnostic {
		for _, id := range target.ScanSources[target.ScanOffset:min(target.ScanOffset+1, len(target.ScanSources))] {
			found := false
			for _, source := range sources {
				if source.ID == id {
					batch = append(batch, source)
					found = true
					break
				}
			}
			if !found {
				scanProcessed++
				_ = s.repo.Event(ctx, BorrowEvent{TargetID: target.AccountID, SourceID: id, Stage: "diagnostic", Reason: "source_skipped"})
			}
		}
	} else {
		for offset := 0; offset < len(sources); offset++ {
			batch = append(batch, sources[(int(target.Cursor)*policy.Concurrency+offset)%len(sources)])
		}
	}
	batch = borrowReplenishmentBatch(batch, replenishment, policy.Concurrency)
	// A lease/budget race may defer a source after the snapshot check. Refill
	// that worker from remaining eligible sources without increasing concurrency.
	jobs := make(chan BorrowSource, len(batch))
	for i, source := range batch {
		if replenishment != nil && policy.Concurrency > 1 && i == 0 && source.ID == replenishment.SourceID {
			continue
		}
		jobs <- source
	}
	close(jobs)
	for worker := range min(policy.Concurrency, len(batch)) {
		workerJobs := jobs
		if replenishment != nil && policy.Concurrency > 1 && worker == 0 && len(batch) > 0 && batch[0].ID == replenishment.SourceID {
			dedicated := make(chan BorrowSource, 1)
			dedicated <- batch[0]
			close(dedicated)
			workerJobs = dedicated
		}
		acquiring.Add(1)
		go func() {
			defer acquiring.Done()
			for source := range workerJobs {
				if ctx.Err() != nil {
					return
				}
				details := &BorrowCandidateDetails{CandidateID: uuid.NewString(), BatchID: lease}
				if replenishment != nil && source.ID == replenishment.SourceID {
					details.Replenishment = &BorrowReplenishmentAudit{EnvironmentVersion: replenishment.EnvironmentVersion, Until: &replenishment.Until}
				}
				if r, ok := s.repo.(BorrowInventoryRepository); ok {
					needed, e := r.NeedsCandidate(ctx, acquisition, nil)
					if e != nil || !needed {
						return
					}
				}
				sourceCtx := ctx
				stopSource := func() {}
				if replenishment != nil && source.ID == replenishment.SourceID {
					sourceCtx, stopSource = context.WithDeadline(ctx, replenishment.Until)
				}
				if replenishment != nil && policy.Concurrency == 1 && source.ID != replenishment.SourceID {
					// Single-worker exploration is a short slice, so a slow
					// alternate does not consume the retained source's window.
					sourceCtx, stopSource = context.WithTimeout(ctx, time.Duration(max(1, replenishment.IntervalSeconds))*time.Second)
				}
				cookie, expires, e := s.acquireSourceObserved(sourceCtx, source, settings, details)
				stopSource()
				if errors.Is(e, errBorrowSourceDeferred) {
					continue
				}
				acquiredMu.Lock()
				if ctx.Err() == nil {
					acquiredCount++
				}
				acquiredMu.Unlock()
				if e == nil {
					candidates <- candidate{source: source.ID, cookie: cookie, expires: expires, details: details}
				}
				return
			}
		}()
	}

	type verification struct {
		candidate candidate
		result    *OpenAICodexStateProbeResult
		reason    string
		terminal  bool
		scanned   bool
	}
	var seenMu sync.Mutex
	seen := make(map[string]bool)
	verify := func(c candidate) verification {
		out := verification{candidate: c}
		latestSettings, e := s.repo.Settings(ctx)
		if e != nil || !latestSettings.Running {
			out.reason, out.terminal = "borrow_acquisition_paused", true
			return out
		}
		current, e := s.repo.Target(ctx, target.AccountID)
		if e != nil || !current.Enabled || current.Revision != target.Revision {
			out.reason, out.terminal = "target_unavailable", true
			return out
		}
		if e := s.repo.CheckAcquisition(ctx, acquisition); e != nil {
			out.reason, out.terminal = borrowAcquisitionReason(e), true
			return out
		}
		hash := borrowHash(c.cookie.Value)
		seenMu.Lock()
		duplicate := seen[hash]
		seen[hash] = true
		seenMu.Unlock()
		known, e := s.repo.CandidateKnown(ctx, target.AccountID, identity, hash)
		if e != nil || known || duplicate {
			out.reason = "candidate_store_unavailable"
			if known || duplicate {
				out.reason, out.scanned = "candidate_duplicate", true
			}
			return out
		}
		now, e := s.repo.Now(ctx)
		if e != nil || !now.Before(c.expires) {
			out.reason = "candidate_expired"
			if e != nil {
				out.reason = "candidate_store_unavailable"
			}
			return out
		}
		if latestSettings.MinRouteRemainingSeconds != nil && !now.Add(time.Duration(*latestSettings.MinRouteRemainingSeconds)*time.Second).Before(c.expires) {
			out.reason = "candidate_publish_expired"
			return out
		}
		if r, ok := s.repo.(BorrowInventoryRepository); ok {
			needed, e := r.NeedsCandidate(ctx, acquisition, &c.expires)
			if e != nil {
				out.reason = borrowAcquisitionReason(e)
				return out
			}
			if !needed {
				out.reason = "candidate_inventory_sufficient"
				return out
			}
		}
		// Probe capacity is independent of business admission. The repository also
		// reserves each HTTP attempt under the target lock across all server instances.
		_ = s.repo.SetPhase(ctx, target.AccountID, lease, "verifying")
		probeCtx, stop := context.WithTimeout(ctx, min(90*time.Second, c.expires.Sub(now)))
		defer stop()
		beforeProbe := c.details.snapshot(c.expires, now)
		c.details.VerificationPreparedAt = &now
		c.details.CookieAgeAtProbeMS = beforeProbe.CookieAgeMS
		c.details.RemainingAtProbeMS = beforeProbe.RemainingMS
		probeCtx = context.WithValue(probeCtx, borrowProbeRequestKey{}, BorrowRequest{Kind: "probe", TargetID: target.AccountID, SourceID: c.source, BatchID: lease, Revision: target.Revision, CandidateID: c.details.CandidateID})
		probeCtx = context.WithValue(probeCtx, borrowBeforeSendKey{}, template.Context().Value(borrowBeforeSendKey{}))
		probeCtx = context.WithValue(probeCtx, openAICodexShotObserverKey{}, s.borrowShotObserver(account, targetModel, strings.TrimPrefix(template.Header.Get("Authorization"), "Bearer ")))
		req := template.Clone(probeCtx)
		replaceBorrowCookie(req, c.cookie)
		out.result = ProbeOpenAICodexStateRoute(probeCtx, s.owner.httpUpstream, req, proxy, target.AccountID, policy.Concurrency, profile, targetModel)
		result := out.result
		c.details.ProbeVerdict = string(result.Verdict)
		c.details.ProbeFailure = result.Failure
		c.details.ProbeRetries = result.RetryCount
		c.details.MintStatus, c.details.ContinueStatus = result.MintStatus, result.ContinueStatus
		return out
	}
	results := make(chan verification, len(batch))
	var verifying sync.WaitGroup
	for range min(policy.Concurrency, len(batch)) {
		verifying.Add(1)
		go func() {
			defer verifying.Done()
			for c := range candidates {
				results <- verify(c)
			}
		}()
	}
	go func() { acquiring.Wait(); close(candidates) }()
	go func() { verifying.Wait(); close(results) }()
	defer func() {
		// Release source leases and probe bodies before ending the target lease.
		cancel()
		acquiring.Wait()
		verifying.Wait()
		cleanup, finish := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer finish()
		for out := range results {
			// A concurrent winner must not hide an explicit upstream restriction
			// already returned by another candidate before cancellation.
			if out.result != nil {
				waitReason, wait := borrowProbeUpstreamWait(out.result)
				if wait > 0 {
					_ = s.repo.RecordProbeOutcome(cleanup, target.AccountID, lease, waitReason, wait)
					emitCandidate(out.candidate, "target", waitReason)
					continue
				}
			}
			outcome := "candidate_unused_after_stop"
			if reason == "target_probe_passed" {
				outcome = "candidate_unused_after_success"
			}
			emitCandidate(out.candidate, "candidate", outcome)
		}
		if diagnostic && scanProcessed == 0 && reason == "no_candidate" && acquiredCount > 0 {
			scanProcessed = 1
		}
	}()
	for out := range results {
		c, result := out.candidate, out.result
		if result == nil {
			if diagnostic && out.scanned {
				scanProcessed = 1
			}
			emitCandidate(c, "candidate", out.reason)
			if out.terminal {
				reason = out.reason
				break
			}
			continue
		}
		if result.Failure == "probe_busy" || result.Failure == "recovery_waiting" || result.Failure == "request_budget" || result.Failure == "route_ready" {
			reason = map[string]string{"probe_busy": "borrow_probe_concurrency_full", "recovery_waiting": "target_upstream_waiting", "request_budget": "target_request_budget", "route_ready": "target_route_ready"}[result.Failure]
			emitCandidate(c, "candidate", reason)
			continue
		}
		if result.Failure == "acquisition_paused" {
			reason = "borrow_acquisition_paused"
			emitCandidate(c, "candidate", reason)
			break
		}
		if replenishment != nil && result.Failure == OpenAICodexStateFailureCancelled {
			// A bounded window ending is a truncation, not a failed ticket or a
			// seven-second continuation timeout. Explicit 401/429 remain above
			// this classification in the probe result.
			reason = "candidate_unused_after_stop"
			emitCandidate(c, "candidate", reason)
			return
		}
		if diagnostic {
			scanProcessed = 1
		}
		reason = "target_probe_inconclusive"
		if result.Verdict == OpenAICodexStateDegraded {
			reason = "target_ticket_changed"
		}
		if result.Failure == "route_changed" {
			reason = "target_route_changed"
		}
		if result.Failure == borrowContinueTimeoutReason {
			reason = borrowContinueTimeoutReason
			// Persist across batches, scoped to this target and identity only.
			if e := s.repo.RejectCandidate(ctx, target.AccountID, identity, borrowHash(c.cookie.Value), c.expires); e != nil {
				reason = "candidate_store_unavailable"
				emitCandidate(c, "target", reason)
				return
			}
		}
		if result.Verdict == OpenAICodexStateDegraded || result.Failure == "route_changed" {
			_ = s.repo.RejectCandidate(ctx, target.AccountID, identity, borrowHash(c.cookie.Value), c.expires)
		}
		if result.Verdict == OpenAICodexStateHealthy {
			if !result.RouteCookieDeadline.IsZero() {
				deadline, e := s.repo.ObserveCookie(ctx, borrowHash(c.cookie.Value), time.Until(result.RouteCookieDeadline))
				if e != nil {
					reason = "target_probe_inconclusive"
					emitCandidate(c, "target", reason)
					continue
				}
				c.expires = minBorrowTime(c.expires, deadline)
			}
			if expiry := account.GetOpenAITokenExpiresAt(); expiry != nil && expiry.Before(c.expires) {
				c.expires = *expiry
			}
			route := &BorrowRoute{CandidateID: c.details.CandidateID, SourceSubjectHash: c.details.SourceSubjectHash, TargetSubjectHash: borrowSubjectHash(account), SourceRevision: c.details.SourceRevision, SourceEnvironmentVersion: c.details.SourceEnvironmentVersion, SourceRotationRevision: c.details.SourceRotationRevision, ID: uuid.NewString(), TargetID: target.AccountID, TargetRevision: target.Revision, SourceID: c.source, IdentityHash: identity, CookieHash: borrowHash(c.cookie.Value), Gateway: borrowGateway(c.cookie.Value), ExpiresAt: c.expires, Secret: BorrowRouteSecret{Endpoint: template.URL.Scheme + "://" + template.URL.Host + template.URL.EscapedPath(), Cookie: *c.cookie, Headers: template.Header.Clone(), Proxy: proxy, Profile: profile}}
			route.Secret.Headers.Set("Cookie", result.RouteCookie)
			publicationStarted := time.Now()
			err = s.repo.Publish(ctx, route, lease)
			c.details.PublicationMS = time.Since(publicationStarted).Milliseconds()
			if err == nil {
				c.routeID = route.ID
				reason = "target_probe_passed"

				delay = 0
			} else {
				reason = "candidate_publish_failed"
				switch {
				case errors.Is(err, ErrBorrowConflict):
					reason = "candidate_publish_conflict"
				case errors.Is(err, ErrBorrowCandidateDuplicate):
					reason = "candidate_duplicate"
				case errors.Is(err, ErrBorrowCandidateExpiry):
					reason = "candidate_publish_expired"
				}
			}
		}
		if result.Verdict == OpenAICodexStateHealthy || result.Verdict == OpenAICodexStateDegraded || result.Failure == borrowContinueTimeoutReason || result.Failure == "route_changed" {
			if r, ok := s.repo.(BorrowCandidateRepository); ok {
				recordCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
				recordErr := r.RecordCandidate(recordCtx, acquisition, c.details.CandidateID, borrowHash(c.cookie.Value), string(result.Verdict), reason, c.expires)
				stop()
				if recordErr != nil {
					reason = "candidate_store_unavailable"
				}
			}
		}
		if replenishment != nil && c.source == replenishment.SourceID && (reason == "target_ticket_changed" || reason == "target_route_changed") {
			if r, ok := s.repo.(BorrowReplenishmentRepository); ok {
				if e := r.EndReplenishment(ctx, acquisition, c.source, reason); e != nil {
					reason = "source_store_unavailable"
					return
				}
			}
		}
		waitReason, recoveryDelay := borrowProbeUpstreamWait(result)
		if recoveryDelay > 0 {
			reason = waitReason
		}
		// The 150-second batch deadline may fire while a probe is reading. Its
		// outcome still needs to enter recovery under the same fenced lease.
		outcomeCtx, finishOutcome := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		outcomeErr := s.repo.RecordProbeOutcome(outcomeCtx, target.AccountID, lease, reason, recoveryDelay)
		finishOutcome()
		if outcomeErr != nil {
			reason = "source_store_unavailable"
			emitCandidate(c, "target", reason)
			break
		}
		emitCandidate(c, "target", reason)
		if result.Failure == OpenAICodexStateFailureRateLimited {
			delay = time.Minute
			if result.RetryAfter > delay {
				delay = result.RetryAfter
			}
			reason = "target_rate_limited"
			return
		}
		if result.Failure == OpenAICodexStateFailureAccountError {
			delay = recoveryDelay
			reason = "target_credentials_unavailable"
			return
		}
		if ctx.Err() != nil {
			// Cleanup and FinishJob have independent bounded contexts. Do not use
			// an expired work context for another read and call it a store outage.
			return
		}
		if reason != "target_probe_passed" && !settings.Replenishment.Allows(target.AccountID) {
			if e := s.repo.CheckAcquisition(ctx, acquisition); e != nil {
				reason = borrowAcquisitionReason(e)
				return
			}
		}
		// Drain acquisitions before ending the leased job; no orphaned source work.
		if reason == "target_probe_passed" && !settings.Replenishment.Allows(target.AccountID) {
			// The route is already published and recovery state persisted. Cancel
			// speculative source work before draining so a slow exit cannot retain
			// shared source/target leases until the whole batch deadline.
			cancel()
			return
		}
	}
}
func (s *TicketBorrowService) acquireSource(ctx context.Context, source BorrowSource, settings BorrowSettings) (cookie *http.Cookie, expires time.Time, err error) {
	return s.acquireSourceObserved(ctx, source, settings, &BorrowCandidateDetails{CandidateID: uuid.NewString()})
}

func (s *TicketBorrowService) acquireSourceObserved(ctx context.Context, source BorrowSource, settings BorrowSettings, details *BorrowCandidateDetails) (cookie *http.Cookie, expires time.Time, err error) {
	ctx = WithHTTPUpstreamRedirectsDisabled(context.WithValue(ctx, borrowInternalKey{}, true))
	var cookieObserved time.Time
	if a, ok := BorrowAcquisitionFromContext(ctx); ok {
		if e := s.repo.CheckAcquisition(ctx, a); e != nil {
			return nil, expires, errBorrowSourceDeferred
		}
	}
	lease := uuid.NewString()
	permitRepo, fair := s.repo.(BorrowSourcePermitRepository)
	_, hasBatch := BorrowAcquisitionFromContext(ctx)
	fair = fair && hasBatch
	grant := func() error {
		queuedAt := time.Now()
		defer func() { details.SourceQueueMS += time.Since(queuedAt).Milliseconds() }()
		if !fair {
			ok, e := s.repo.AcquireJob(ctx, "source", source.ID, lease)
			if e != nil {
				return e
			}
			if !ok {
				return errBorrowSourceDeferred
			}
			return nil
		}
		if e := permitRepo.QueueSourceAttempt(ctx, source.ID, lease); e != nil {
			return e
		}
		defer func() {
			cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer stop()
			_ = permitRepo.CancelSourceAttempt(cleanup, lease)
		}()
		for {
			ok, e := permitRepo.GrantSourceAttempt(ctx, source.ID, lease)
			if e != nil {
				return e
			}
			if ok {
				return nil
			}
			timer := time.NewTimer(250 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	if e := grant(); e != nil {
		outcome := borrowAcquisitionReason(e)
		if errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) {
			outcome = "source_acquisition_cancelled"
		}
		s.candidateEvent(ctx, BorrowEvent{SourceID: source.ID, Stage: "source", Reason: outcome}, details, expires)
		if errors.Is(e, ErrBorrowRequestBudget) {
			return nil, expires, e
		}
		return nil, expires, errBorrowSourceDeferred
	}
	reason := "source_failed"
	delay := time.Duration(0)
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer stop()
		if reason == "source_failed" && (errors.Is(ctx.Err(), context.Canceled) || (details.Replenishment != nil && errors.Is(ctx.Err(), context.DeadlineExceeded))) {
			reason = "source_acquisition_cancelled"
		}
		if reason == "source_http_401" || reason == "source_http_403" || reason == "source_http_429" || reason == "source_rate_limited" || reason == "source_credentials_unavailable" {
			if r, ok := s.repo.(BorrowReplenishmentRepository); ok {
				if a, exists := BorrowAcquisitionFromContext(ctx); exists {
					_ = r.EndReplenishment(cleanup, a, source.ID, reason)
				}
			}
		}
		if fair {
			_ = permitRepo.SourceAttemptCompleted(cleanup, lease)
		}
		_ = s.repo.FinishJob(cleanup, "source", source.ID, lease, reason, delay)
		s.candidateEvent(cleanup, BorrowEvent{SourceID: source.ID, Stage: "source", Reason: reason}, details, expires)
	}()
	if fair {
		until, e := permitRepo.SourceHoldUntil(ctx, source.ID)
		if e != nil {
			return nil, expires, e
		}
		if until != nil {
			heldCtx, stop := context.WithDeadline(ctx, *until)
			defer stop()
			ctx = heldCtx
			if details.Replenishment == nil {
				details.Replenishment = &BorrowReplenishmentAudit{Until: until}
			}
		}
	}
	preparationStarted := time.Now()
	sourcePtr, err := s.repo.Source(ctx, source.ID)
	if err != nil {
		return nil, expires, err
	}
	if sourcePtr == nil {
		return nil, expires, ErrBorrowUnavailable
	}
	source = *sourcePtr
	if !source.Enabled || source.AccountID <= 0 {
		reason = "source_account_unavailable"
		return nil, expires, errBorrowSourceDeferred
	}
	if !source.UseProxyPool || source.PoolEntryID == 0 || len(source.Secret.Proxies) != 1 || strings.TrimSpace(source.Secret.Proxies[0]) == "" {
		reason = "source_proxy_pool_unavailable"
		return nil, expires, errBorrowSourceDeferred
	}
	f := source.Secret.Fingerprint
	if ValidateBorrowFingerprint(f) != nil || f.DeviceID != source.Secret.DeviceID {
		reason = "borrow_fingerprint_invalid"
		return nil, expires, ErrBorrowFingerprintInvalid
	}
	details.SourceAccountID, details.SourceRevision = source.AccountID, source.Revision
	details.SourceEnvironmentVersion, details.SourceRotationRevision = source.EnvironmentVersion, source.RotationRevision
	normalAccount, err := s.normalSourceAccount(ctx, source)
	if err != nil {
		reason = "source_account_unavailable"
		return nil, expires, err
	}
	gateway := s.owner.openaiGatewayService
	token, _, err := gateway.GetAccessToken(ctx, normalAccount)
	if err != nil {
		reason = "source_refresh_failed"
		return nil, expires, err
	}
	normalAccount, err = s.normalSourceAccount(ctx, source)
	if err != nil {
		reason = "source_account_unavailable"
		return nil, expires, err
	}
	details.SourceSubjectHash = borrowSubjectHash(normalAccount)
	proxy, resourceID, concurrency := source.Secret.Proxies[0], normalAccount.ID, 1
	outboundModel := normalizeOpenAIModelForUpstream(normalAccount, normalAccount.GetMappedModel(source.Model))
	details.SourceModel, details.SourceFormat = outboundModel, "probe_minimal"
	headers, err := gateway.buildOpenAICodexProbeHeaders(ctx, normalAccount, token, outboundModel)
	if err != nil {
		return nil, expires, err
	}
	f.Apply(headers, outboundModel)
	profile := f.Profile
	ctx = context.WithValue(ctx, openAICodexShotObserverKey{}, s.borrowShotObserver(normalAccount, outboundModel, token))
	details.SourcePreparationMS = time.Since(preparationStarted).Milliseconds()
	for attempt := 0; attempt < 3; attempt++ {
		var reserveErr error
		if fair {
			if attempt > 0 {
				lease = uuid.NewString()
				reserveErr = grant()
			}
		} else {
			reserveErr = s.repo.ReserveSourceAttempt(ctx, source.ID, lease)
		}
		if e := reserveErr; e != nil {
			reason = borrowAcquisitionReason(e)
			if errors.Is(e, ErrBorrowRequestBudget) {
				var wait *BorrowBudgetWaitError
				if errors.As(e, &wait) && wait.Scope == "source" {
					reason = "source_request_budget"
					if wait.RetryAfter > 0 {
						delay = wait.RetryAfter
					}
				}
				return nil, expires, errBorrowSourceDeferred
			}
			if errors.Is(e, ErrBorrowPaused) || errors.Is(e, ErrBorrowRouteReady) || errors.Is(e, ErrBorrowRecoveryWaiting) || errors.Is(e, ErrBorrowUnavailable) {
				return nil, expires, errBorrowSourceDeferred
			}
			if errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) {
				reason = "source_acquisition_cancelled"
			}
			return nil, expires, e
		}
		details.SourceAttempts = attempt + 1
		networkStarted := time.Now()
		shot, e := fireOpenAICodexStateShotRequest(ctx, headers, outboundModel, "", "", func(req *http.Request) (*http.Response, error) {
			details.Source = borrowEnvironment(req, proxy, profile)
			if source.UseProxyPool && source.Secret.Fingerprint != nil {
				details.Source.TLSConfigHash, details.Source.TLSStructureHash = BorrowTLSFingerprintHashes(profile)
				details.Source.FingerprintGenerator = source.Secret.Fingerprint.Generator
			}
			if a, ok := BorrowAcquisitionFromContext(ctx); ok {
				req = req.WithContext(context.WithValue(req.Context(), borrowBeforeSendKey{}, func(check context.Context) error { return s.repo.CheckAcquisition(check, a) }))
			}
			if source.AccountID > 0 {
				// Selection/role/account changes are checked for every retry and once more
				// after DNS/TLS/connection waiting, just before bytes can leave the process.
				prior, _ := req.Context().Value(borrowBeforeSendKey{}).(func(context.Context) error)
				req = req.WithContext(context.WithValue(req.Context(), borrowBeforeSendKey{}, func(checkCtx context.Context) error {
					if prior != nil {
						if e := prior(checkCtx); e != nil {
							return e
						}
					}
					latest, e := s.repo.Source(checkCtx, source.ID)
					if e != nil || !latest.Enabled || latest.Model != source.Model ||
						latest.UseProxyPool != source.UseProxyPool || latest.RotateIdentity != source.RotateIdentity ||
						latest.PoolEntryID != source.PoolEntryID || latest.Secret.DeviceID != source.Secret.DeviceID ||
						len(latest.Secret.Proxies) != 1 || latest.Secret.Proxies[0] != proxy ||
						!sameBorrowFingerprint(latest.Secret.Fingerprint, source.Secret.Fingerprint) {
						return ErrBorrowUnavailable
					}
					a, e := s.normalSourceAccount(checkCtx, *latest)
					if e != nil || borrowAccountIdentity(a) != borrowAccountIdentity(normalAccount) ||
						borrowCredential(a.Credentials, "access_token") != borrowCredential(normalAccount.Credentials, "access_token") {
						return ErrBorrowUnavailable
					}
					return nil
				}))
			}
			if e := BorrowBeforeSend(req); e != nil {
				reason = "source_configuration_changed"
				return nil, e
			}
			if fair {
				prior, _ := req.Context().Value(borrowBeforeSendKey{}).(func(context.Context) error)
				req = req.WithContext(context.WithValue(req.Context(), borrowBeforeSendKey{}, func(sendCtx context.Context) error {
					if prior != nil {
						if e := prior(sendCtx); e != nil {
							return e
						}
					}
					return permitRepo.SourceAttemptSent(sendCtx, lease)
				}))
			}
			resp, sendErr := s.owner.httpUpstream.DoWithTLS(req, proxy, resourceID, concurrency, profile)

			if resp != nil {
				for _, c := range resp.Cookies() {
					if c.Name == "__oailb" {
						copy := *c
						cookie = &copy
						cookieObserved = time.Now()
					}
				}
			}
			return resp, sendErr
		})

		details.SourceNetworkMS += time.Since(networkStarted).Milliseconds()
		if fair {
			finishCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			_ = permitRepo.SourceAttemptCompleted(finishCtx, lease)

			stop()
		}

		if e == nil && shot.status == 200 && shot.streamErr == nil && shot.failure == "" && cookie != nil && cookie.Value != "" && cookie.MaxAge >= 0 && borrowGateway(cookie.Value) != "" {
			details.CookieHash = borrowHash(cookie.Value)
			details.Gateway = borrowGateway(cookie.Value)
			observed := cookieObserved
			details.CookieObservedAt = &observed
			ttl := borrowCookieTTL(cookie, cookieObserved, time.Now(), settings.TTLSeconds)
			if ttl <= 0 {
				return nil, expires, ErrBorrowUnavailable
			}
			expires, err = s.repo.ObserveCookie(ctx, borrowHash(cookie.Value), ttl)
			if err == nil {
				now, e := s.repo.Now(ctx)
				if e != nil {
					err = e
				} else if !now.Before(expires) {
					err = ErrBorrowUnavailable
				}
			}
			if err == nil {
				reason = "source_cookie_acquired"
			}
			return cookie, expires, err
		}
		cookie = nil
		if reason == "source_configuration_changed" {
			return nil, expires, errBorrowSourceDeferred
		}

		if shot.status == http.StatusUnauthorized || shot.status == http.StatusForbidden {
			reason = fmt.Sprintf("source_http_%d", shot.status)
			break
		}
		if shot.status == http.StatusTooManyRequests || shot.failure == OpenAICodexStateFailureRateLimited {
			reason = "source_rate_limited"
			if shot.status == http.StatusTooManyRequests {
				reason = "source_http_429"
			}
			break
		}
		if shot.failure == OpenAICodexStateFailureAccountError {
			reason = "source_credentials_unavailable"
			break
		}
		if !openAICodexShotRetryable(ctx, shot, e, false) || ctx.Err() != nil || attempt == 2 {
			break
		}
		if fair {
			releaseCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			_ = s.repo.FinishJob(releaseCtx, "source", source.ID, lease, "source_attempt_finished", 0)
			stop()
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 300 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, expires, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, expires, ErrBorrowUnavailable
}

// Keep supported TLS parameters intact while varying their preference order.
// This is an account/round identity, never regenerated per HTTP retry.
func borrowSourceProfile(device string) *tlsfingerprint.Profile {
	ciphers := []uint16{0x1301, 0x1302, 0x1303, 0xc02b, 0xc02f, 0xc02c, 0xc030, 0xcca9, 0xcca8, 0xc009, 0xc013, 0xc00a, 0xc014, 0x009c, 0x009d, 0x002f, 0x0035}
	seed := sha256.Sum256([]byte(device))
	for i := len(ciphers) - 1; i > 3; i-- {
		j := 3 + int(seed[i])%(i-2)
		ciphers[i], ciphers[j] = ciphers[j], ciphers[i]
	}
	return &tlsfingerprint.Profile{Name: "borrow-" + borrowHash(device)[:12], CipherSuites: ciphers}
}

func borrowCookieTTL(cookie *http.Cookie, observed, now time.Time, seconds int) time.Duration {
	deadline := observed.Add(time.Duration(seconds) * time.Second)
	if cookie.MaxAge > 0 {
		deadline = minBorrowTime(deadline, observed.Add(time.Duration(cookie.MaxAge)*time.Second))
	}
	if !cookie.Expires.IsZero() {
		deadline = minBorrowTime(deadline, cookie.Expires)
	}
	return deadline.Sub(now)
}
func minBorrowTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
func borrowRetryAfter(raw string, now time.Time) time.Duration {
	if n, e := strconv.Atoi(strings.TrimSpace(raw)); e == nil && n > 0 {
		// Clamp before converting to nanoseconds; a large upstream integer must
		// not overflow into a negative delay and make acquisition retry early.
		return time.Duration(min(n, 24*60*60)) * time.Second
	}
	if until, e := http.ParseTime(raw); e == nil && until.After(now) {
		return min(until.Sub(now), 24*time.Hour)
	}
	return time.Minute
}

func (s *TicketBorrowService) TransferPreview(ctx context.Context, id int64) (map[string]any, error) {
	a, err := s.owner.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if a == nil || !a.IsOpenAIOAuthLike() || a.ParentAccountID != nil {
		return nil, errors.New("borrow_target_unsupported")
	}
	model := "gpt-6-luna"
	if s.owner.cfg != nil {
		if m := s.owner.cfg.AstraRouting(ctx).CookiePool.SourceAccountModels[id]; m != "" {
			model = m
		}
	}
	return map[string]any{"account_id": id, "name": a.Name, "model": model, "has_proxy": openAIAccountProxyURL(a) != "", "schedulable": a.Schedulable}, nil
}

func (s *TicketBorrowService) targetIdentity(a *Account) string {
	base := borrowAccountIdentity(a)
	if s.owner != nil && s.owner.tlsFPProfileService != nil {
		profile := s.owner.tlsFPProfileService.ResolveTLSProfile(a)
		if profile != nil {
			raw, _ := json.Marshal(profile)
			return borrowHash(base + string(raw))
		}
	}
	return base
}
