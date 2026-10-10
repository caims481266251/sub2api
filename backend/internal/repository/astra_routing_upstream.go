package repository

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/mihomo"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Each settings revision gets a fresh pool. In-flight requests finish against
// their snapshot; removed candidates cannot re-populate the new pool.
type astraRoutingUpstream struct {
	borrowRoutes    *service.TicketBorrowService
	rotationCache   astraRotationCache
	listNodes       func() ([]mihomo.AstraNode, error)
	pinNode         func(context.Context, mihomo.AstraNode) (string, func(), error)
	prepareMu       sync.Mutex
	preparer        func(context.Context, int64) error
	retryAfter      time.Time
	preparing       atomic.Bool
	delegate        service.HTTPUpstream
	cfg             *config.Config
	mu              sync.Mutex
	revision        string
	pool            *codexGatewayPinUpstream
	historyRecorder func(service.AstraGatewayHistoryRecord, bool)
}

func (s *astraRoutingUpstream) SetAstraGatewayHistoryRecorder(fn func(service.AstraGatewayHistoryRecord, bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.historyRecorder = fn
}

func (s *astraRoutingUpstream) current(ctx context.Context) *codexGatewayPinUpstream {
	settings := s.cfg.AstraRouting(ctx)
	poolSettings := settings
	poolSettings.AccountScheduling = false // Scheduling does not invalidate verified routes.
	poolSettings.SchedulingMode = ""
	poolSettings.SchedulingGroupIDs = nil
	encoded, _ := json.Marshal(poolSettings)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !settings.CookiePool.Enabled {
		if s.pool != nil && s.pool.seamless != nil {
			s.pool.seamless.cancel()
		}
		s.pool = nil
		s.revision = ""
		return nil
	}
	if s.pool == nil || s.revision != string(encoded) {
		if s.pool != nil && s.pool.seamless != nil {
			s.pool.seamless.cancel()
		}
		s.revision = string(encoded)
		s.pool = &codexGatewayPinUpstream{rotationCache: &s.rotationCache, delegate: s.delegate, config: settings.CookiePool, historyRecorder: s.historyRecorder}
		if settings.CookiePool.Seamless {
			s.pool.seamless = newAstraSeamlessPool()
			ttl := settings.CookiePool.TTLSeconds
			if ttl == 0 {
				ttl = 230
			}
			s.pool.seamless.prewarm = min(astraSeamlessPrewarm, time.Duration(ttl)*time.Second*2/3)
		}
	}
	return s.pool
}
func (s *astraRoutingUpstream) Do(r *http.Request, p string, a int64, n int) (*http.Response, error) {
	return s.DoWithTLS(r, p, a, n, nil)
}
func (s *astraRoutingUpstream) DoWithTLS(r *http.Request, p string, a int64, n int, f *tlsfingerprint.Profile) (*http.Response, error) {
	if s.borrowRoutes != nil && r != nil {
		enabled, err := s.borrowRoutes.Enabled(r.Context(), a)
		if err != nil {
			return nil, service.ErrBorrowUnavailable
		}
		if enabled || a < 0 {
			return s.delegate.DoWithTLS(r, p, a, n, f)
		}
	}
	if r != nil && !service.IsOpenAICodexStateProbeRequest(r.Context()) && codexGatewayPinRequest(r) {
		if pool := s.current(r.Context()); pool != nil {
			if pool.seamless != nil && service.IsAstraSourceAcquisition(r.Context()) && slices.Contains(pool.config.SourceAccountIDs, a) {
				if err := pool.seamless.consume(true, a, time.Now()); err != nil {
					return nil, err
				}
			}
			if slices.Contains(pool.config.TargetAccountIDs, a) && codexGatewayPinAstra(r) {
				cookie, _, effectiveProxy, release, err := s.targetRoute(r, p, a, n, f)
				if err != nil {
					return nil, err
				}
				if cookie == nil {
					release()
					return s.delegate.DoWithTLS(r, p, a, n, f)
				}
				r = r.Clone(service.WithHTTPUpstreamRedirectsDisabled(r.Context()))
				replaceCodexGatewayCookie(r, cookie)
				if pool.config.RotateNodes {
					r = astraFreshRequest(r)
				}
				resp, err := s.delegate.DoWithTLS(r, effectiveProxy, a, n, f)
				if pool.seamless != nil {
					pool.seamless.observeBusiness(r, p, a, f, cookie, resp)
				}
				return attachAstraLease(resp, err, release)
			}
			return pool.DoWithTLS(r, p, a, n, f)
		}
	}
	return s.delegate.DoWithTLS(r, p, a, n, f)
}
func (s *astraRoutingUpstream) CodexGatewayPinWSHeader(a int64) (string, error) {
	if pool := s.current(context.Background()); pool != nil {
		value, err := pool.CodexGatewayPinWSHeader(a)
		if err != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
			defer cancel()
			if e := s.PrepareAstraGateway(ctx); e != nil {
				return "", e
			}
			pool = s.current(ctx)
			if pool == nil {
				return "", nil
			}
			return pool.CodexGatewayPinWSHeader(a)
		}
		return value, nil
	}
	return "", nil
}

func (s *astraRoutingUpstream) SetAstraGatewayPreparer(fn func(context.Context, int64) error) {
	s.prepareMu.Lock()
	s.preparer = fn
	s.prepareMu.Unlock()
}
func (s *astraRoutingUpstream) PrepareAstraGateway(ctx context.Context) error {
	return s.prepareAstraGateway(ctx, false)
}

func (s *astraRoutingUpstream) prepareAstraGateway(ctx context.Context, refresh bool) error {
	if !s.prepareMu.TryLock() {
		return errors.New("preparation_in_progress")
	}
	defer s.prepareMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	pool := s.current(ctx)
	if pool == nil {
		return errors.New("cookie_pool_disabled")
	}
	if pool.seamless != nil {
		return s.acquireSeamlessSources(ctx, pool)
	}
	if pool.config.RotateNodes {
		return s.prepareRotatedSource(ctx, pool)
	}
	if cookie, _ := pool.currentCookie("/backend-api/codex/responses", time.Now()); cookie != nil && !refresh {
		pool.mu.Lock()
		expires := pool.routes[pool.activeSource].expires
		pool.mu.Unlock()
		// Keep a primary route while its remaining window is healthy; once
		// inside the prewarm window, continue probing for a backup route.
		if time.Until(expires) > 90*time.Second {
			return nil
		}
	}
	if s.preparer == nil {
		return errors.New("source_preparer_unavailable")
	}
	if time.Now().Before(s.retryAfter) {
		return errors.New("source_probe_cooldown")
	}
	s.preparing.Store(true)
	defer s.preparing.Store(false)
	// Keep all completed candidates: an acquisition success alone does not
	// prove that the target can use this route. Bound parallel upstream work.
	var workers sync.WaitGroup
	slots := make(chan struct{}, 4)
	for _, id := range pool.config.SourceAccountIDs {
		pool.mu.Lock()
		blocked := time.Now().Before(pool.rejectedSources[id])
		if refresh {
			if pool.statuses == nil {
				pool.statuses = make(map[int64]service.AstraRouteStatus)
			}
			status := pool.statuses[id]
			status.AccountID = id
			status.State = "waiting"
			status.Reason = "reacquiring_after_target_failure"
			pool.statuses[id] = status
		}
		pool.mu.Unlock()
		if blocked && !refresh {
			continue
		}
		workers.Add(1)
		go func(id int64) {
			defer workers.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			if ctx.Err() != nil || s.current(ctx) != pool {
				return
			}
			probe, cancel := context.WithTimeout(ctx, 45*time.Second)
			defer cancel()
			probeStart := time.Now()
			if err := s.preparer(probe, id); err != nil {
				reason := "source_test_failed"
				if err.Error() == "answer_mismatch" {
					reason = "candy_answer_mismatch"
				}
				pool.recordSourceReason(id, probeStart, nil, false, reason)
			}
		}(id)
	}
	workers.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if s.current(ctx) != pool {
		return errors.New("configuration_changed")
	}
	if cookie, _ := pool.currentCookie("/backend-api/codex/responses", time.Now()); cookie != nil {
		s.retryAfter = time.Time{}
		return nil
	}
	s.retryAfter = time.Now().Add(15 * time.Second)
	return errors.New("no_qualified_source_route")
}
func (s *astraRoutingUpstream) AstraGatewaySnapshot(ctx context.Context) service.AstraGatewayRuntime {
	settings := s.cfg.AstraRouting(ctx)
	result := service.AstraGatewayRuntime{GeneratedAt: time.Now(), Revision: settings.Revision, Sources: []service.AstraRouteStatus{}, Targets: []service.AstraRouteStatus{}, WS: []service.AstraWSStatus{}, Preparing: s.preparing.Load()}
	pool := s.current(ctx)
	if pool == nil {
		for _, id := range settings.CookiePool.SourceAccountIDs {
			result.Sources = append(result.Sources, service.AstraRouteStatus{AccountID: id, State: "disabled", Reason: "disabled"})
		}
		return result
	}
	if pool.seamless != nil {
		pool.mu.Lock()
		result.Gateways = pool.gatewayObservations()
		result.UnknownGatewaySamples = pool.unknownGatewaySamples
		pool.mu.Unlock()
		result = pool.seamless.snapshot(result, pool.config, time.Now())
		pool.mu.Lock()
		for i, row := range result.Sources {
			if row.State == "waiting" {
				if prior, ok := pool.statuses[row.AccountID]; ok {
					result.Sources[i] = prior
				}
			}
		}
		pool.mu.Unlock()
		return result
	}
	now := time.Now()
	// Never hold a lock for the duration of a network validation. Snapshot a
	// coherent route/check pair; only an exact current Cookie can be ready.
	pool.targetMu.Lock()
	pool.mu.Lock()
	defer pool.targetMu.Unlock()
	defer pool.mu.Unlock()
	result.Cooldowns = s.rotationCache.snapshot(settings.CookiePool.TargetAccountIDs, now)
	result.Gateways = pool.gatewayObservations()
	result.UnknownGatewaySamples = pool.unknownGatewaySamples
	readySource := map[int64]time.Time{}
	failedSource := map[int64]bool{}
	for _, id := range settings.CookiePool.TargetAccountIDs {
		row := service.AstraRouteStatus{AccountID: id, State: "waiting", Reason: "target_not_verified"}
		if check, ok := pool.targetChecks[id]; ok {
			checked := check.checked
			row.CheckedAt = &checked
			row.Gateway = check.gateway
			row.ProxyNode = check.node.Name
			row.ProxyCountry = check.node.Country
			route, exists := pool.routes[check.sourceID]
			current := exists && sha256.Sum256([]byte(route.cookie.Value)) == check.cookieFingerprint && now.Before(route.expires) && now.Before(check.expires) && (!pool.config.RotateNodes || route.node.Identity == check.node.Identity) && (!pool.config.IPAffinity || pool.config.RotateNodes || check.proxyFingerprint == sha256.Sum256([]byte(route.proxy)))
			if current && check.passed {
				expiry := check.expires
				if route.expires.Before(expiry) {
					expiry = route.expires
				}
				row.State = "ready"
				row.Reason = "target_probe_passed"
				row.ExpiresAt = &expiry
				row.RemainingSeconds = max(0, int64(expiry.Sub(now).Seconds()))
				if old, ok := readySource[check.sourceID]; !ok || expiry.Before(old) {
					readySource[check.sourceID] = expiry
				}
			} else if current {
				row.State = "rejected"
				row.Answer = check.answer
				row.Reason = check.reason
				failedSource[check.sourceID] = true
			} else if !check.passed && now.Before(check.expires) {
				row.State = "rejected"
				row.Reason = check.reason
			} else {
				row.State = "waiting"
				row.Reason = "target_not_verified"
			}
		}
		result.Targets = append(result.Targets, row)
	}
	for _, id := range pool.config.SourceAccountIDs {
		row := service.AstraRouteStatus{AccountID: id, State: "waiting", Reason: "not_tested"}
		if prior, ok := pool.statuses[id]; ok {
			row.CheckedAt = prior.CheckedAt
			row.State = prior.State
			row.Reason = prior.Reason
			row.Gateway = prior.Gateway
		}
		if route, ok := pool.routes[id]; ok {
			row.Gateway = astraRoutingHost(route.cookie.Value)
			row.ProxyNode = route.node.Name
			row.ProxyCountry = route.node.Country
			switch {
			case !now.Before(route.expires):
				row.State = "expired"
				row.Reason = "route_expired"
			case !readySource[id].IsZero():
				expiry := readySource[id]
				row.ExpiresAt = &expiry
				row.RemainingSeconds = max(0, int64(expiry.Sub(now).Seconds()))
				row.State = "ready"
				row.Reason = "target_probe_passed"
				row.Active = pool.activeSource == id
				result.ReadyRoutes++
			case failedSource[id]:
				row.State = "candidate"
				row.Reason = "source_passed_target_failed"
			default:
				row.State = "candidate"
				row.Reason = "source_passed_pending_target"
			}
		}
		result.Sources = append(result.Sources, row)
	}
	return result
}

// Only a hostname matching the known gateway namespace is exposed. JWT decoding
// is descriptive metadata, not signature verification or proof of actual routing.
var astraGatewayHostPattern = regexp.MustCompile(`^chat\.gateway\.unified-[0-9]+\.api\.openai\.com$`)

func astraRoutingHost(value string) string {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Host string `json:"host"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	if !astraGatewayHostPattern.MatchString(claims.Host) {
		return ""
	}
	return claims.Host
}
