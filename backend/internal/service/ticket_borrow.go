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
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/google/uuid"
)

type borrowInternalKey struct{}
type borrowPreparedKey struct{}
type borrowBeforeSendKey struct{}
type borrowSourceSentKey struct{}

// Invoked only after request headers have been written. Validation hooks can run
// before DNS or connection waits and must not count an unsent candidate as used.
func BorrowSourceRequestSent(req *http.Request) error {
	if req == nil {
		return nil
	}
	if record, ok := req.Context().Value(borrowSourceSentKey{}).(func(context.Context) error); ok {
		return record(req.Context())
	}
	return nil
}

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
	responseBindings sync.Map // pending metadata writes; deltas never wait for these
	poolSources      atomic.Int32
	routeNotifyMu    sync.Mutex
	routeNotify      chan struct{}
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
	if v.RenewBeforeSeconds != nil && (*v.RenewBeforeSeconds < 0 || *v.RenewBeforeSeconds > 240) {
		return ErrBorrowProbePolicyInvalid
	}
	previous, err := s.repo.Settings(ctx)
	if err != nil {
		return err
	}
	if err := ResolveBorrowAdmissionSettings(&v, previous); err != nil {
		return err
	}
	if v.Recovery == nil {
		v.Recovery = previous.Recovery
	}
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
	if err := s.validateTarget(ctx, id, enabled); err != nil {
		return err
	}
	return s.repo.SetTarget(ctx, id, enabled, revision)
}
func (s *TicketBorrowService) validateTarget(ctx context.Context, id int64, enabled bool) error {
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
	return nil
}
func (s *TicketBorrowService) Status(ctx context.Context, id int64) (BorrowTarget, error) {
	target, err := s.repo.Target(ctx, id)
	if err != nil || (!target.Enabled && target.Revision == 0) {
		return target, err
	}
	if !target.Enabled {
		if allocator, ok := s.repo.(BorrowRouteLeaseRepository); ok {
			target.BusinessRoutes, err = allocator.RouteStatuses(ctx, target.AccountID, "")
			if err != nil {
				return target, err
			}
			for _, route := range target.BusinessRoutes {
				target.BusinessInflight += route.Inflight
			}
		}
		if s.owner != nil && s.owner.accountRepo != nil {
			if account, e := s.owner.accountRepo.GetByID(ctx, target.AccountID); e == nil && account != nil {
				target.BusinessConcurrency = account.Concurrency
			}
		}
		return target, nil
	}
	a, err := s.owner.accountRepo.GetByID(ctx, target.AccountID)
	if err != nil {
		return target, err
	}
	if a != nil {
		target.BusinessConcurrency = a.Concurrency
	}
	if a == nil || !a.IsSchedulable() {
		if allocator, ok := s.repo.(BorrowRouteLeaseRepository); ok {
			target.BusinessRoutes, err = allocator.RouteStatuses(ctx, target.AccountID, "")
			if err != nil {
				return target, err
			}
			for _, route := range target.BusinessRoutes {
				target.BusinessInflight += route.Inflight
			}
		}
		if target.Enabled {
			target.BorrowInventory = &BorrowInventory{BlockedReason: "target_unavailable"}
		}
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
	target.PreferredRouteID = route.ID
	target.Gateway = route.Gateway
	target.ExpiresAt = &route.ExpiresAt
	if !route.UsableUntil.IsZero() {
		target.UsableUntil = &route.UsableUntil
	}
	return target, nil
}
func (s *TicketBorrowService) Targets(ctx context.Context, includeDisabled ...bool) ([]BorrowTarget, error) {
	targets, err := s.repo.Targets(ctx)
	if len(includeDisabled) > 0 && includeDisabled[0] {
		if r, ok := s.repo.(BorrowAdminTargetRepository); ok {
			targets, err = r.AdminTargets(ctx)
		}
	}
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
		return borrowAdmissionError(err)
	}
	if !t.Enabled {
		if continuation != nil || borrowPreferredPass(ctx) {
			return ErrBorrowUnavailable
		}
		return nil
	}
	// Model availability and mapping are enforced by ordinary account selection.
	// Borrowing adds route admission, not a separate model allowlist.
	if strings.TrimSpace(model) == "" || (transport != "http" && transport != "ws") {
		return ErrBorrowUnavailable
	}
	a, err := s.owner.accountRepo.GetByID(ctx, t.AccountID)
	if err != nil {
		return borrowAdmissionError(err)
	}
	if a == nil || !a.IsSchedulable() {
		return ErrBorrowUnavailable
	}
	_, identity, err := s.targetEnvironment(ctx, a)
	if err != nil {
		return borrowAdmissionError(err)
	}
	if continuation != nil {
		if err := s.waitResponseBinding(ctx, id, continuation.Response); err != nil {
			return err
		}
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
	return borrowAdmissionError(err)
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
		return nil, borrowAdmissionError(err)
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
		Reasoning          struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	err = json.NewDecoder(io.LimitReader(body, 32<<20)).Decode(&payload)
	body.Close()
	if err != nil || strings.TrimSpace(payload.Model) == "" {
		return nil, ErrBorrowUnavailable
	}
	if err := BorrowBusinessRecoveryFromContext(req.Context()).Activate(id, continuation != nil || payload.PreviousResponseID != ""); err != nil {
		return nil, err
	}
	account, err := s.owner.accountRepo.GetByID(req.Context(), target.AccountID)
	if err != nil {
		return nil, borrowAdmissionError(err)
	}
	if account == nil || !account.IsSchedulable() {
		return nil, borrowPreSendRejection(ErrBorrowUnavailable)
	}
	_, identity, err := s.targetEnvironment(req.Context(), account)
	if err != nil {
		return nil, borrowPreSendRejection(err)
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
		if err := s.waitResponseBinding(req.Context(), id, continuation.Response); err != nil {
			return nil, err
		}
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
		return nil, borrowPreSendRejection(err)
	}
	use := newBorrowRouteUse(req.Context(), allocator, leaseID, 15*time.Second)
	bodyOwnsLease := false
	defer func() {
		if !bodyOwnsLease {
			use.finish()
		}
	}()
	req = req.WithContext(use.ctx)
	if route.Secret.Endpoint == "" || req.URL.Scheme+"://"+req.URL.Host+req.URL.EscapedPath() != route.Secret.Endpoint {
		return nil, borrowPreSendRejection(ErrBorrowUnavailable)
	}
	r := req.Clone(context.WithValue(req.Context(), borrowPreparedKey{}, true))
	// Business keeps the normal gateway transport profile and connection pool.
	// Fresh randomized connections belong to acquisition/probes, not user traffic.
	r = r.WithContext(context.WithValue(r.Context(), borrowBeforeSendKey{}, func(ctx context.Context) error {
		latest, e := s.owner.accountRepo.GetByID(ctx, target.AccountID)
		if e != nil {
			return borrowAdmissionError(e)
		}
		if latest == nil || !latest.IsSchedulable() {
			return ErrBorrowUnavailable
		}
		_, identity, e := s.targetEnvironment(ctx, latest)
		if e != nil {
			return borrowAdmissionError(e)
		}
		if e := allocator.RenewRoute(ctx, leaseID); e != nil {
			return borrowAdmissionError(e)
		}
		_, e = s.repo.Route(ctx, target.AccountID, identity, route.ID)
		return borrowAdmissionError(e)
	}))
	r = r.WithContext(context.WithValue(r.Context(), borrowHTTPTransportStartedKey{}, &atomic.Bool{}))
	// Keep the normal business identity. Probe headers and TLS never enter this request.
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
	r = r.WithContext(requesttiming.WithBorrowRoute(r.Context(), route.ID))
	r = r.WithContext(requesttiming.WithBorrowRequest(r.Context(), payload.Model, payload.Reasoning.Effort))
	resp, err := send(r, openAIAccountProxyURL(account), id, n, profile)
	if resp != nil {
		s.observeBorrowBusinessResponse(r, route, resp)
		// Admission deadlines govern new requests only. Preserve this response
		// and its lease through normal streaming, error handling and accounting;
		// never replay a request because its returned routing metadata changed.
		if err == nil && resp.Body != nil && resp.Body != http.NoBody {
			observed := &borrowObservedBody{ReadCloser: resp.Body, owner: s, target: target.AccountID, route: route.ID}
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

// Routing metadata alone does not revoke a validated route or replace its shared
// Cookie. Authentication keeps the ordinary policy.
func (s *TicketBorrowService) observeBorrowBusinessResponse(req *http.Request, route *BorrowRoute, resp *http.Response) {
	if reason := borrowResponseRoutingChange(req, route, resp); reason != "" && resp.StatusCode != http.StatusUnauthorized {
		slog.InfoContext(req.Context(), "borrow business response routing metadata changed; preserving current response",
			"target_id", route.TargetID, "route_id", route.ID, "reason", reason)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(req.Context()), 3*time.Second)
		defer cancel()
		const reason = "target_unauthorized"
		_ = s.repo.Revoke(ctx, route.TargetID, route.ID, reason)
		_ = s.repo.RefreshTarget(ctx, route.TargetID)
		_ = s.repo.Event(ctx, BorrowEvent{TargetID: route.TargetID, SourceID: route.SourceID, RouteID: route.ID, Stage: "route", Reason: reason})
	}
}

func borrowResponseRoutingChange(req *http.Request, route *BorrowRoute, resp *http.Response) string {
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
	ctx, cancelCause := context.WithCancelCause(context.Background())
	s.cancel = func() { cancelCause(borrowCancelServiceShutdown) }
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
	if pool, ok := s.repo.(BorrowCandidatePoolRepository); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = pool.SweepCandidatePool(ctx)
		cancel()
	}
}
func (s *TicketBorrowService) processRotations(context.Context) bool { return true }
func (s *TicketBorrowService) tick(ctx context.Context) {
	s.cleanupEvents(ctx)
	if pool, ok := s.repo.(BorrowCandidatePoolRepository); ok {
		if err := pool.SweepCandidatePool(ctx); err != nil {
			// Do not add work while ownership/capacity recovery is unavailable.
			return
		}
	}
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
	s.startPoolProducers(ctx)
}
func (s *TicketBorrowService) runTarget(parent context.Context, target BorrowTarget, settings BorrowSettings, lease string) {
	diagnostic := target.ScanOffset < len(target.ScanSources)
	pool, hasPool := s.repo.(BorrowCandidatePoolRepository)
	sharedPool := hasPool && target.AcquisitionMode() == BorrowAcquisitionSource && !diagnostic
	ctx, cancelCause := context.WithCancelCause(context.WithValue(parent, borrowInternalKey{}, true))
	reason := "no_candidate"
	cancel := func() {
		switch reason {
		case "target_probe_passed":
			cancelCause(borrowCancelBatchSuccess)
		case "borrow_acquisition_paused":
			cancelCause(borrowCancelUserStop)
		default:
			cancelCause(context.Canceled)
		}
	}
	defer cancel()
	delay := time.Duration(0)
	scanProcessed := 0
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
		defer stop()
		if context.Cause(ctx) == borrowCancelTargetLeaseLost {
			reason = string(borrowCancelTargetLeaseLost)
		}
		if diagnostic && scanProcessed > 0 {
			_ = s.repo.AdvanceScan(cleanup, target.AccountID, lease, scanProcessed)
		}
		_ = s.repo.FinishJob(cleanup, "target", target.AccountID, lease, reason, delay)
	}()
	stopLease, leaseErr := s.keepTargetLease(ctx, BorrowAcquisition{TargetID: target.AccountID, Revision: target.Revision, Lease: lease}, cancelCause)
	if leaseErr != nil {
		reason = borrowAcquisitionReason(leaseErr)
		return
	}
	defer stopLease()
	var sources []BorrowSource
	var err error
	if target.AcquisitionMode() == BorrowAcquisitionSelf {
		diagnostic = false
		sources = []BorrowSource{{AccountID: target.AccountID, Model: "gpt-6-luna", Enabled: true, UseProxyPool: true}}
	} else if !sharedPool {
		sources, err = s.repo.Sources(ctx)
		if err != nil {
			reason = "source_store_unavailable"
			return
		}
		enabled := sources[:0]
		for _, source := range sources {
			if source.UnavailableReason == "source_proxy_pool_unavailable" {
				reason = source.UnavailableReason
			}
			if source.Enabled && source.AccountID > 0 && !source.NextAttemptAt.After(time.Now()) && (source.LeaseUntil == nil || !source.LeaseUntil.After(time.Now())) {
				if _, e := s.normalSourceAccount(ctx, source); e == nil {
					enabled = append(enabled, source)
				}
			}
		}
		sources = enabled
	}
	if !sharedPool && len(sources) == 0 {
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
	_, identity, err := s.targetEnvironment(ctx, account)
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
	var profile *tlsfingerprint.Profile
	if s.owner.tlsFPProfileService != nil {
		profile = s.owner.tlsFPProfileService.ResolveTLSProfile(account)
	}
	// Fence probe dispatch after connection waits and before retries/publication.
	expectedIdentity := identity
	expectedTargetCredential := borrowHash(borrowCredential(account.Credentials, "access_token"))
	template = template.WithContext(context.WithValue(template.Context(), borrowBeforeSendKey{}, func(check context.Context) error {
		latest, e := s.normalBorrowAccount(check, target.AccountID, OpenAICodexStateProbeDefaultModel)
		if e != nil || latest == nil {
			return ErrBorrowUnavailable
		}
		_, current, e := s.targetEnvironment(check, latest)
		if e != nil || current != expectedIdentity || borrowHash(borrowCredential(latest.Credentials, "access_token")) != expectedTargetCredential {
			return ErrBorrowUnavailable
		}
		return nil
	}))
	targetModel := normalizeOpenAIModelForUpstream(account, account.GetMappedModel(OpenAICodexStateProbeDefaultModel))
	policy := target.ProbePolicy.effective()
	ctx = withBorrowProbeSlots(ctx, policy.Concurrency)
	type candidate = borrowProbeCandidate
	targetEnvironment := borrowEnvironment(template, proxy, profile)
	targetEnvironment.BodyHash = borrowHash(string(openAICodexStateProbeBody(targetModel)))
	emitCandidate := func(c candidate, stage, outcome string) {
		c.details.Business = targetEnvironment
		c.details.TargetIdentityHash = identity
		c.details.TargetRevision = target.Revision
		s.candidateEvent(ctx, BorrowEvent{TargetID: target.AccountID, SourceID: c.source, RouteID: c.routeID, Stage: stage, Reason: outcome}, c.details, c.expires)
	}
	var acquiredCount int
	var acquiredMu sync.Mutex
	var allocated sync.Map
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer stop()
		if r, ok := s.repo.(BorrowCandidateEnvironmentRepository); ok {
			allocated.Range(func(id, _ any) bool { _ = r.ReleaseCandidateEnvironment(cleanup, id.(string)); return true })
		}
	}()
	// Diagnostic runs one source at a time so a cooldown/pause cannot silently
	// advance past unverified candidates from an already acquired batch.

	batch := make([]BorrowSource, 0, policy.Concurrency)
	if target.AcquisitionMode() == BorrowAcquisitionSelf {
		for range policy.Concurrency {
			batch = append(batch, sources[0])
		}
	} else if diagnostic {
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
	type verification = borrowProbeVerification
	var seenMu sync.Mutex
	seen := make(map[string]time.Time)
	verify := func(c candidate) verification {
		// Preparation and HTTP validation share one per-candidate budget.
		ctx, stopVerification := context.WithTimeoutCause(ctx, BorrowCandidateVerificationLimit, borrowCancelValidationDeadline)
		defer stopVerification()
		out := verification{candidate: c}
		latestSettings, e := s.repo.Settings(ctx)
		if e != nil || !latestSettings.Running {
			if e == nil {
				cancelCause(borrowCancelUserStop)
				c.details.CancellationCause = string(borrowCancelUserStop)
			}
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
		now, e := s.repo.Now(ctx)
		clockReadAt := time.Now()
		if e != nil || !now.Before(c.expires) {
			out.reason = "candidate_expired"
			if e != nil {
				out.reason = "candidate_store_unavailable"
			}
			return out
		}
		hash := borrowHash(c.cookie.Value)
		seenMu.Lock()
		// A task can outlive many candidates; do not retain expired Cookies.
		for key, until := range seen {
			if !until.After(now) {
				delete(seen, key)
			}
		}
		_, duplicate := seen[hash]
		seen[hash] = c.expires
		seenMu.Unlock()
		known, e := s.repo.CandidateKnown(ctx, target.AccountID, identity, hash)
		if e != nil || known || duplicate {
			out.reason = "candidate_store_unavailable"
			if known || duplicate {
				out.reason, out.scanned = "candidate_duplicate", true
			}
			return out
		}
		if c.details.AcquisitionStartedAt != nil {
			c.expires = borrowAdmissionDeadline(*c.details.AcquisitionStartedAt, latestSettings.AdmissionSeconds(), c.expires)
			out.candidate.expires = c.expires
			if !now.Before(c.expires) {
				out.reason = "candidate_publish_expired"
				return out
			}
		}
		if sharedPool && c.expires.Sub(now) < BorrowPoolSettingsBudget(latestSettings, "mint") {
			out.reason = "candidate_time_budget_insufficient"
			return out
		}
		// Shared claims already passed demand admission. A concurrent success
		// must not cancel this claimed verification just because inventory filled.
		if r, ok := s.repo.(BorrowInventoryRepository); ok && !sharedPool {
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
		probeCtx, stop := context.WithDeadlineCause(ctx, clockReadAt.Add(c.expires.Sub(now)), borrowCancelCandidateDeadline)
		defer stop()
		if sharedPool {
			probeCtx = context.WithValue(probeCtx, borrowPoolVerificationKey{}, borrowPoolVerificationDeadline{Until: c.expires})
		}
		beforeProbe := c.details.snapshot(c.expires, now)
		c.details.VerificationPreparedAt = &now
		if c.details.SourceAcquiredAt != nil && !sharedPool {
			wait := now.Sub(*c.details.SourceAcquiredAt).Milliseconds()
			if wait < 0 {
				wait = 0
			}
			c.details.ProbeQueueMS = &wait
		}
		c.details.CookieAgeAtProbeMS = beforeProbe.CookieAgeMS
		c.details.RemainingAtProbeMS = beforeProbe.RemainingMS
		probeCtx = context.WithValue(probeCtx, borrowProbeRequestKey{}, BorrowRequest{Kind: "probe", TargetID: target.AccountID, SourceID: c.source, BatchID: lease, Revision: target.Revision, CandidateID: c.details.CandidateID})
		probeCtx = context.WithValue(probeCtx, borrowBeforeSendKey{}, func(check context.Context) error {
			if before, ok := template.Context().Value(borrowBeforeSendKey{}).(func(context.Context) error); ok {
				if e := before(check); e != nil {
					return e
				}
			}
			if c.details.checkSource != nil {
				if err := c.details.checkSource(check); err != nil {
					return err
				}
			}
			r, ok := s.repo.(BorrowCandidateEnvironmentRepository)
			if !ok || c.details.Environment == nil {
				return ErrBorrowUnavailable
			}
			return r.CheckCandidateEnvironment(check, c.details.Environment.ID)
		})
		probeCtx = context.WithValue(probeCtx, openAICodexShotObserverKey{}, s.borrowShotObserver(account, targetModel, strings.TrimPrefix(template.Header.Get("Authorization"), "Bearer ")))
		req := template.Clone(probeCtx)
		env := c.details.Environment
		if env == nil {
			out.reason = "source_proxy_pool_unavailable"
			return out
		}
		env.Fingerprint.Apply(req.Header, targetModel)
		replaceBorrowCookie(req, c.cookie)
		c.details.Target = borrowEnvironment(req, env.Proxy, env.Fingerprint.Profile)
		c.details.Target.TLSConfigHash, c.details.Target.TLSStructureHash = BorrowTLSFingerprintHashes(env.Fingerprint.Profile)
		c.details.Target.FingerprintGenerator = env.Fingerprint.Generator
		out.result = ProbeOpenAICodexStateRoute(probeCtx, s.owner.httpUpstream, req, env.Proxy, target.AccountID, policy.Concurrency, env.Fingerprint.Profile, targetModel)
		result := out.result
		c.details.TargetBalancerHash = result.RouteBalancerHash
		c.details.TargetCookiePairHash = result.RouteCookiePairHash
		c.details.MintTicketHash = result.MintTicketHash
		c.details.ContinueTicketHash = result.ContinueTicketHash
		c.details.ProbeVerdict = string(result.Verdict)
		c.details.ProbeFailure = result.Failure
		c.details.ProbeRetries = result.RetryCount
		c.details.Mint, c.details.Continue = result.Mint, result.Continue
		c.details.CancellationCause = borrowCancellation(probeCtx, nil)
		for _, phase := range []*BorrowProbePhase{result.Mint, result.Continue} {
			if phase != nil && len(phase.Attempts) > 0 {
				last := phase.Attempts[len(phase.Attempts)-1]
				if last.CancellationCause != "" {
					c.details.CancellationCause = last.CancellationCause
				}
			}
		}
		c.details.MintStatus, c.details.ContinueStatus = result.MintStatus, result.ContinueStatus
		return out
	}
	feedback, _ := s.repo.(BorrowGatewayFeedbackRepository)
	if s.owner.cfg != nil && s.owner.cfg.Gateway.BorrowAdaptiveProbingDisabled {
		feedback = nil
	}
	if !sharedPool && len(batch) == 0 {
		return
	}
	capacity := policy.Concurrency
	if diagnostic {
		capacity = 1
	}
	releaseCandidate := func(c candidate) {
		if c.details == nil || c.details.Environment == nil {
			return
		}
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer stop()
		if r, ok := s.repo.(BorrowCandidateEnvironmentRepository); ok {
			if e := r.ReleaseCandidateEnvironment(cleanup, c.details.Environment.ID); e == nil {
				allocated.Delete(c.details.Environment.ID)
			}
		}
	}
	processed := false
	consume := func(out verification) bool {
		processed = true
		defer releaseCandidate(out.candidate)
		c, result := out.candidate, out.result
		if result == nil {
			reason = out.reason
			if diagnostic && out.scanned {
				scanProcessed = 1
			}
			emitCandidate(c, "candidate", out.reason)
			if out.terminal {
				reason = out.reason
				return false
			}
			return true
		}
		if result.Failure == "candidate_time_budget_insufficient" {
			reason = result.Failure
			emitCandidate(c, "candidate", reason)
			return true
		}
		if result.Failure == "probe_busy" || result.Failure == "recovery_waiting" || result.Failure == "request_budget" || result.Failure == "route_ready" {
			reason = map[string]string{"probe_busy": "borrow_probe_concurrency_full", "recovery_waiting": "target_upstream_waiting", "request_budget": "target_request_budget", "route_ready": "target_route_ready"}[result.Failure]
			emitCandidate(c, "candidate", reason)
			return true
		}
		if result.Failure == "acquisition_paused" {
			cancelCause(borrowCancelUserStop)
			c.details.CancellationCause = string(borrowCancelUserStop)
			reason = "borrow_acquisition_paused"
			emitCandidate(c, "candidate", reason)
			return false
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
				return false
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
					return true
				}
				c.expires = minBorrowTime(c.expires, deadline)
			}
			if expiry := account.GetOpenAITokenExpiresAt(); expiry != nil && expiry.Before(c.expires) {
				c.expires = *expiry
			}
			route := &BorrowRoute{AcquisitionIdentity: target.AcquisitionMode(), AcquisitionAccountID: c.details.SourceAccountID, AcquisitionStartedAt: c.details.AcquisitionStartedAt, AdmissionUntil: c.expires, CandidateID: c.details.CandidateID, SourceSubjectHash: c.details.SourceSubjectHash, SourceCredentialHash: c.details.SourceCredentialHash, TargetSubjectHash: borrowSubjectHash(account), SourceRevision: c.details.SourceRevision, SourceEnvironmentVersion: c.details.SourceEnvironmentVersion, SourceRotationRevision: c.details.SourceRotationRevision, ID: uuid.NewString(), TargetID: target.AccountID, TargetRevision: target.Revision, SourceID: c.source, IdentityHash: identity, CookieHash: borrowHash(c.cookie.Value), Gateway: borrowGateway(c.cookie.Value), ExpiresAt: c.expires, Secret: BorrowRouteSecret{Endpoint: template.URL.Scheme + "://" + template.URL.Host + template.URL.EscapedPath(), Cookie: *c.cookie, Headers: template.Header.Clone(), Proxy: proxy, Profile: profile}}
			if c.details.UpstreamExpiresAt != nil {
				route.ExpiresAt = *c.details.UpstreamExpiresAt
			}
			route.Secret.Headers.Set("Cookie", result.RouteCookie)
			publicationStarted := time.Now()
			err = nil
			if before, ok := template.Context().Value(borrowBeforeSendKey{}).(func(context.Context) error); ok {
				err = before(ctx)
			}
			if err == nil {
				err = c.details.checkSource(ctx)
			}
			if err == nil {
				err = s.repo.(BorrowCandidateEnvironmentRepository).CheckCandidateEnvironment(ctx, c.details.Environment.ID)
			}
			if err == nil {
				err = s.repo.Publish(ctx, route, lease)
				if err != nil && sharedPool {
					// A committed transaction can lose its acknowledgement. Query
					// its stable candidate key; never repeat upstream verification.
					check, done := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
					published, checkErr := pool.PublishedPoolCandidate(check, acquisition, c.details.CandidateID)
					done()
					if checkErr == nil && published != "" {
						route.ID, err = published, nil
					}
				}
			}
			c.details.PublicationMS = time.Since(publicationStarted).Milliseconds()
			if err == nil {
				s.notifyRoutePublished()
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
		if feedback != nil && c.details.AcquisitionStartedAt != nil && BorrowGatewayFeedbackReason(reason) {
			feedbackCtx, finish := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			feedbackErr := feedback.RecordBorrowGatewayOutcome(feedbackCtx, acquisition, BorrowGatewayOutcome{
				CandidateID: c.details.CandidateID, CookieHash: borrowHash(c.cookie.Value), Gateway: c.details.Gateway,
				StartedAt: *c.details.AcquisitionStartedAt, Reason: reason,
			})
			finish()
			if feedbackErr != nil {
				slog.Warn("borrow_gateway_feedback_write_failed", "target_id", target.AccountID)
				emitCandidate(c, "candidate", "gateway_feedback_unavailable")
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
		waitReason, recoveryDelay := borrowProbeUpstreamWait(result)
		if recoveryDelay > 0 {
			reason = waitReason
		}
		// Persist completed outcomes even when the task was cancelled while
		// reading; cleanup still uses the same fenced lease.
		outcomeCtx, finishOutcome := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		outcomeErr := s.repo.RecordProbeOutcome(outcomeCtx, target.AccountID, lease, reason, recoveryDelay)
		finishOutcome()
		if outcomeErr != nil {
			reason = "source_store_unavailable"
			emitCandidate(c, "target", reason)
			return false
		}
		emitCandidate(c, "target", reason)
		if result.Failure == OpenAICodexStateFailureRateLimited {
			delay = time.Minute
			if result.RetryAfter > delay {
				delay = result.RetryAfter
			}
			reason = "target_rate_limited"
			return false
		}
		if result.Failure == OpenAICodexStateFailureAccountError {
			delay = recoveryDelay
			reason = "target_credentials_unavailable"
			return false
		}
		if ctx.Err() != nil {
			// Cleanup and FinishJob have independent bounded contexts. Do not use
			// an expired work context for another read and call it a store outage.
			return false
		}
		if reason != "target_probe_passed" && !settings.Replenishment.Allows(target.AccountID) {
			if e := s.repo.CheckAcquisition(ctx, acquisition); e != nil {
				reason = borrowAcquisitionReason(e)
				return false
			}
		}
		// Drain acquisitions before ending the leased job; no orphaned source work.
		if reason == "target_probe_passed" && !settings.Replenishment.Allows(target.AccountID) && !sharedPool {
			// The route is already published and recovery state persisted. Cancel
			// speculative source work before draining so a slow exit cannot retain
			// shared source/target leases after demand has closed.
			cancelCause(borrowCancelBatchSuccess)
			return false
		}
		return true
	}
	needed := func() (bool, error) {
		if e := s.repo.CheckAcquisition(ctx, acquisition); e != nil {
			return false, e
		}
		if r, ok := s.repo.(BorrowInventoryRepository); ok {
			inv, e := r.Inventory(ctx, acquisition.TargetID, acquisition.Identity)
			if e != nil {
				return false, e
			}
			// Both modes follow current demand, never the task's initial state.
			// A leased explicit refresh may take one wave while healthy.
			return inv.Count < inv.Desired || inv.State == "renew" || !processed, nil
		}
		return true, nil
	}
	if sharedPool {
		ctx = context.WithValue(ctx, borrowGatewayFeedbackDisabledKey{}, feedback == nil)
		poolReason := s.consumeCandidatePool(ctx, pool, acquisition, capacity, needed, verify, func(out verification) (bool, string) {
			keep := consume(out)
			return keep, reason
		})
		if poolReason != "" {
			reason = poolReason
		}
		return
	}
	pipeline := borrowCandidatePipeline{
		capacity: capacity, continuous: feedback != nil && !diagnostic, sources: batch,
		needed: func() bool {
			need, e := needed()
			if e != nil {
				reason = borrowAcquisitionReason(e)
			}
			return e == nil && need
		},
		acquire: func(source BorrowSource) (candidate, error) {
			details := &BorrowCandidateDetails{CandidateID: uuid.NewString(), BatchID: lease}
			cookie, expires, e := s.acquireSourceObserved(ctx, source, settings, details)
			acquiredMu.Lock()
			if !errors.Is(e, errBorrowSourceDeferred) && ctx.Err() == nil {
				acquiredCount++
			}
			acquiredMu.Unlock()
			if e == nil {
				allocated.Store(details.Environment.ID, true)
			}
			return candidate{source: source.ID, cookie: cookie, expires: expires, details: details}, e
		},
		verify: verify, consume: consume,
		priorities: func() map[string]time.Time {
			if feedback == nil {
				return nil
			}
			priorities, e := feedback.BorrowGatewayPriorities(ctx, acquisition)
			if e != nil {
				slog.Warn("borrow_gateway_feedback_read_failed", "target_id", target.AccountID)
				_ = s.repo.Event(ctx, BorrowEvent{TargetID: target.AccountID, Stage: "candidate", Reason: "gateway_feedback_unavailable"})
			}
			return priorities
		},
		selected: func(c candidate, demoted, promoted bool) {
			c.details.GatewayPriority = "normal"
			if promoted {
				c.details.GatewayPriority = "prioritized"
				emitCandidate(c, "candidate", "candidate_gateway_prioritized")
			}
			if demoted {
				c.details.GatewayPriority = "recheck"
				emitCandidate(c, "candidate", "candidate_gateway_recheck")
			}
		},
		discard: func(out verification) {
			defer releaseCandidate(out.candidate)
			cleanup, finish := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer finish()
			if out.result != nil {
				waitReason, wait := borrowProbeUpstreamWait(out.result)
				if wait > 0 {
					_ = s.repo.RecordProbeOutcome(cleanup, target.AccountID, lease, waitReason, wait)
					emitCandidate(out.candidate, "target", waitReason)
					return
				}
			}
			out.candidate.details.CancellationCause = borrowCancellation(ctx, nil)
			outcome := "candidate_unused_after_stop"
			if reason == "target_probe_passed" {
				outcome = "candidate_unused_after_success"
			}
			emitCandidate(out.candidate, "candidate", outcome)
		},
	}
	pipeline.run(ctx, cancel)
	if diagnostic && scanProcessed == 0 && reason == "no_candidate" && acquiredCount > 0 {
		scanProcessed = 1
	}
}

func (s *TicketBorrowService) acquireSource(ctx context.Context, source BorrowSource, settings BorrowSettings) (cookie *http.Cookie, expires time.Time, err error) {
	return s.acquireSourceObserved(ctx, source, settings, &BorrowCandidateDetails{CandidateID: uuid.NewString()})
}

func (s *TicketBorrowService) acquireSourceObserved(ctx context.Context, source BorrowSource, settings BorrowSettings, details *BorrowCandidateDetails) (cookie *http.Cookie, expires time.Time, err error) {
	if s.owner == nil || s.owner.httpUpstream == nil {
		return nil, expires, ErrBorrowUnavailable
	}
	ctx = WithHTTPUpstreamRedirectsDisabled(context.WithValue(ctx, borrowInternalKey{}, true))
	acquisition, ok := BorrowAcquisitionFromContext(ctx)
	poolAcquisition, pooled := BorrowPoolAcquisitionFromContext(ctx)
	environments, hasEnvironments := s.repo.(BorrowCandidateEnvironmentRepository)
	permits, hasPermits := s.repo.(BorrowSourcePermitRepository)
	if (!ok && !pooled) || !hasEnvironments || !hasPermits {
		return nil, expires, ErrBorrowUnavailable
	}
	if pooled {
		pool, available := s.repo.(BorrowCandidatePoolRepository)
		if !available || poolAcquisition.ID != details.CandidateID || poolAcquisition.SourceID != source.ID {
			return nil, expires, ErrBorrowUnavailable
		}
		err = pool.CheckCandidatePoolAcquisition(ctx, poolAcquisition)
	} else {
		err = s.repo.CheckAcquisition(ctx, acquisition)
	}
	if err != nil {
		return nil, expires, errBorrowSourceDeferred
	}
	lease := uuid.NewString()
	reason := "source_failed"
	var delay time.Duration
	details.AcquisitionIdentity = BorrowAcquisitionSource
	if source.ID == 0 {
		details.AcquisitionIdentity = BorrowAcquisitionSelf
	}
	details.SourceAccountID = source.AccountID
	var releaseSlot func()
	defer func() {
		if releaseSlot != nil {
			releaseSlot()
		}
	}()
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer stop()
		if details.CancellationCause == "" {
			details.CancellationCause = borrowCancellation(ctx, err)
		}
		_ = permits.CancelSourceAttempt(cleanup, lease)
		_ = permits.SourceAttemptCompleted(cleanup, lease)
		if source.ID > 0 {
			_ = s.repo.FinishJob(cleanup, "source", source.ID, lease, reason, delay)
		}
		if err != nil && details.Environment != nil {
			_ = environments.ReleaseCandidateEnvironment(cleanup, details.Environment.ID)
		}
		s.candidateEvent(cleanup, BorrowEvent{TargetID: acquisition.TargetID, SourceID: source.ID, Stage: "source", Reason: reason}, details, expires)
	}()
	queued := time.Now()
	if err = permits.QueueSourceAttempt(ctx, source.ID, lease); err != nil {
		return nil, expires, err
	}
	for {
		if source.ID == 0 {
			releaseSlot, err = acquireBorrowProbeSlot(ctx, 0)
			if err != nil {
				return nil, expires, err
			}
		}
		var granted bool
		granted, err = permits.GrantSourceAttempt(ctx, source.ID, lease)
		if err != nil {
			reason = borrowAcquisitionReason(err)
			var wait *BorrowBudgetWaitError
			if errors.As(err, &wait) && wait.Scope == "source" {
				delay = wait.RetryAfter
			}
			if errors.Is(err, ErrBorrowRequestBudget) || errors.Is(err, ErrBorrowPaused) || errors.Is(err, ErrBorrowRecoveryWaiting) || errors.Is(err, ErrBorrowUnavailable) || errors.Is(err, ErrBorrowRouteReady) {
				return nil, expires, errBorrowSourceDeferred
			}
			return nil, expires, err
		}
		if granted {
			break
		}
		if releaseSlot != nil {
			releaseSlot()
			releaseSlot = nil
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, expires, ctx.Err()
		case <-timer.C:
		}
	}
	details.SourceQueueMS = time.Since(queued).Milliseconds()
	preparation := time.Now()
	loadAccount := func(check context.Context) (*Account, error) {
		if source.ID == 0 {
			return s.normalBorrowAccount(check, source.AccountID, source.Model)
		}
		latest, e := s.repo.Source(check, source.ID)
		if e != nil {
			return nil, e
		}
		if latest == nil || !latest.Enabled || latest.AccountID != source.AccountID || latest.Model != source.Model || latest.Revision != source.Revision {
			return nil, ErrBorrowUnavailable
		}
		return s.normalSourceAccount(check, *latest)
	}
	account, err := loadAccount(ctx)
	if err != nil {
		reason = "source_account_unavailable"
		return nil, expires, err
	}
	token, _, err := s.owner.openaiGatewayService.GetAccessToken(ctx, account)
	if err != nil {
		reason = "source_refresh_failed"
		return nil, expires, err
	}
	account, err = loadAccount(ctx)
	if err != nil {
		reason = "source_account_unavailable"
		return nil, expires, err
	}
	// Shared source acquisition owns a neutral environment. Ordinary/self
	// acquisition keeps allocation keyed to the existing target identity.
	allocationIdentity := "shared-candidate-pool"
	if !pooled {
		targetAccount, e := s.owner.accountRepo.GetByID(ctx, acquisition.TargetID)
		if e != nil || targetAccount == nil {
			return nil, expires, ErrBorrowUnavailable
		}
		allocationIdentity = borrowProxyIdentity(targetAccount)
	}
	env, err := environments.AllocateCandidateEnvironment(ctx, details.CandidateID, allocationIdentity)
	if err != nil {
		reason = "source_proxy_pool_unavailable"
		return nil, expires, err
	}
	details.Environment = env
	details.PoolEntryID, details.AllocationRound = env.EntryID, env.AllocationRound
	details.ProxySessionRandomized = env.SessionRandomized
	details.SourceRevision = source.Revision
	details.SourceSubjectHash = borrowSubjectHash(account)
	details.SourceCredentialHash = borrowHash(borrowCredential(account.Credentials, "access_token"))
	details.checkSource = func(check context.Context) error {
		latest, err := loadAccount(check)
		if err != nil || latest == nil || borrowSubjectHash(latest) != details.SourceSubjectHash || borrowHash(borrowCredential(latest.Credentials, "access_token")) != details.SourceCredentialHash {
			return ErrBorrowUnavailable
		}
		return nil
	}
	model := normalizeOpenAIModelForUpstream(account, account.GetMappedModel(source.Model))
	details.SourceModel, details.SourceFormat = model, "probe_minimal"
	headers, err := s.owner.openaiGatewayService.buildOpenAICodexProbeHeaders(ctx, account, token, model)
	if err != nil {
		return nil, expires, err
	}
	env.Fingerprint.Apply(headers, model)
	ctx = context.WithValue(ctx, openAICodexShotObserverKey{}, s.borrowShotObserver(account, model, token))
	details.SourcePreparationMS = time.Since(preparation).Milliseconds()
	env.StartedAt, env.AdmissionUntil, err = environments.StartCandidateEnvironment(ctx, env.ID)
	if err != nil {
		return nil, expires, err
	}
	details.AcquisitionStartedAt = &env.StartedAt
	details.AdmissionUntil = &env.AdmissionUntil
	details.SourceAttempts = 1
	networkStarted := time.Now()
	var observed time.Time
	shot, sendErr := fireOpenAICodexStateShotRequest(ctx, headers, model, "", "", func(req *http.Request) (*http.Response, error) {
		details.Source = borrowEnvironment(req, env.Proxy, env.Fingerprint.Profile)
		details.Source.TLSConfigHash, details.Source.TLSStructureHash = BorrowTLSFingerprintHashes(env.Fingerprint.Profile)
		details.Source.FingerprintGenerator = env.Fingerprint.Generator
		req = req.WithContext(context.WithValue(req.Context(), borrowBeforeSendKey{}, func(check context.Context) error {
			if e := environments.CheckCandidateEnvironment(check, env.ID); e != nil {
				return e
			}
			return details.checkSource(check)
		}))
		req = req.WithContext(context.WithValue(req.Context(), borrowSourceSentKey{}, func(sent context.Context) error {
			// Once headers were written, retain history even if the caller cancels.
			record, stop := context.WithTimeout(context.WithoutCancel(sent), 3*time.Second)
			defer stop()
			if e := environments.MarkCandidateEnvironmentSent(record, env.ID); e != nil {
				return e
			}
			return permits.SourceAttemptSent(record, lease)
		}))
		// Transports recheck this hook after connection waits as well.
		if e := BorrowBeforeSend(req); e != nil {
			reason = "source_configuration_changed"
			return nil, e
		}
		resp, e := s.owner.httpUpstream.DoWithTLS(req, env.Proxy, account.ID, 1, env.Fingerprint.Profile)
		if resp != nil {
			balancerHash, routeHash, pairHash := borrowCookieResponseHashes(resp.Cookies())
			if balancerHash != "" {
				details.SourceBalancerHash = balancerHash
			}
			if pairHash != "" {
				details.SourceCookiePairHash = pairHash
			}
			for _, c := range resp.Cookies() {
				if c.Name == "__oailb" {
					copy := *c
					cookie = &copy
					if routeHash != "" {
						details.CookieHash = routeHash
					}
					observed = time.Now()
				}
			}
		}
		return resp, e
	})
	details.SourceNetworkMS = time.Since(networkStarted).Milliseconds()
	details.SourceHTTP = shot.diagnostic
	if shot.diagnostic != nil {
		details.CancellationCause = shot.diagnostic.CancellationCause
	}
	if sendErr == nil && shot.status == 200 && shot.streamErr == nil && shot.failure == "" && cookie != nil && cookie.Value != "" && cookie.MaxAge >= 0 && borrowGateway(cookie.Value) != "" {
		if details.CookieHash == "" {
			details.CookieHash = borrowHash(cookie.Value)
		}
		details.Gateway = borrowGateway(cookie.Value)
		details.CookieObservedAt = &observed
		// Persist the actual Cookie deadline independently from the local cutoff.
		seconds := settings.AdmissionSeconds()
		if cookie.MaxAge > 0 {
			seconds = cookie.MaxAge
		} else if !cookie.Expires.IsZero() {
			seconds = max(1, int(cookie.Expires.Sub(observed).Seconds())+1)
		}
		ttl := borrowCookieTTL(cookie, observed, time.Now(), seconds)
		if ttl <= 0 {
			return nil, expires, ErrBorrowCandidateExpiry
		}
		upstream, e := s.repo.ObserveCookie(ctx, details.CookieHash, ttl)
		if e != nil {
			return nil, expires, e
		}
		if cookie.MaxAge > 0 || !cookie.Expires.IsZero() {
			details.UpstreamExpiresAt = &upstream
		}
		expires = minBorrowTime(env.AdmissionUntil, upstream)
		if expiry := account.GetOpenAITokenExpiresAt(); expiry != nil {
			expires = minBorrowTime(expires, *expiry)
		}
		now, e := s.repo.Now(ctx)
		if e != nil {
			return nil, expires, e
		}
		if !now.Before(expires) {
			return nil, expires, ErrBorrowCandidateExpiry
		}
		reason = "source_cookie_acquired"
		acquired := time.Now()
		details.SourceAcquiredAt = &acquired
		return cookie, expires, nil
	}
	switch {
	case shot.status == 401 || shot.status == 403 || shot.status == 429:
		reason = fmt.Sprintf("source_http_%d", shot.status)
	case shot.status == 429 || shot.failure == OpenAICodexStateFailureRateLimited:
		reason = "source_rate_limited"
	case shot.failure == OpenAICodexStateFailureAccountError:
		reason = "source_credentials_unavailable"
	case ctx.Err() != nil:
		reason = "source_acquisition_cancelled"
	}
	if details.SourceHTTP != nil && details.SourceHTTP.ErrorClass == "" {
		switch {
		case shot.status != 200:
			details.SourceHTTP.ErrorClass = "upstream_http_error"
		case shot.failure != "":
			details.SourceHTTP.ErrorClass = "upstream_stream_error"
		case shot.streamErr != nil:
			details.SourceHTTP.ErrorClass = "incomplete_stream"
		default:
			details.SourceHTTP.ErrorClass = "missing_route_cookie"
		}
		details.SourceHTTP.FailureStage = "response_validation"
	}
	// A retry starts a new candidate, consumes a new permit and gets a fresh environment.
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
