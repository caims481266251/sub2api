package repository

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

const astraSeamlessMargin = 15 * time.Second
const astraSeamlessPrewarm = 150 * time.Second

// Candidate objects are immutable. An acquisition never overwrites a route
// already leased by a business request or validated for a different target.
type astraSeamlessCandidate struct {
	id     [32]byte
	source int64
	route  codexGatewayRoute
}
type astraSeamlessPass struct {
	candidate astraSeamlessCandidate
	checked   time.Time
}
type astraSeamlessIdentity struct {
	target                              int64
	key, auth                           [32]byte
	request                             *http.Request // headers only; never retain a business body/context
	proxy                               string
	concurrency                         int
	profile                             *tlsfingerprint.Profile
	seen                                time.Time
	passes                              []astraSeamlessPass // primary first, at most two
	rejected                            map[[32]byte]time.Time
	running                             bool
	done                                chan struct{}
	cancel                              context.CancelFunc
	next                                time.Time
	failures                            int
	phase, reason, failure              string
	mintStatus, continueStatus, retries int
}
type astraSeamlessPool struct {
	mu            sync.Mutex
	ctx           context.Context
	cancel        context.CancelFunc
	candidates    map[[32]byte]astraSeamlessCandidate
	deadlines     map[[32]byte]time.Time
	identities    map[int64]map[[32]byte]*astraSeamlessIdentity
	canonical     map[int64][32]byte
	auth          map[int64][32]byte
	blocked       map[int64]string
	sourceCursor  int
	requests      []astraSeamlessUsage
	cooling       map[int64]time.Time
	workers       chan struct{}
	targetRunning map[int64]bool
	jobs          sync.WaitGroup
	prewarm       time.Duration
}
type astraSeamlessUsage struct {
	at      time.Time
	source  bool
	account int64
}

func newAstraSeamlessPool() *astraSeamlessPool {
	ctx, cancel := context.WithCancel(context.Background())
	return &astraSeamlessPool{ctx: ctx, cancel: cancel, prewarm: astraSeamlessPrewarm, candidates: map[[32]byte]astraSeamlessCandidate{}, deadlines: map[[32]byte]time.Time{}, identities: map[int64]map[[32]byte]*astraSeamlessIdentity{}, canonical: map[int64][32]byte{}, auth: map[int64][32]byte{}, blocked: map[int64]string{}, cooling: map[int64]time.Time{}, workers: make(chan struct{}, 2), targetRunning: map[int64]bool{}}
}

func (p *astraSeamlessPool) record(source int64, route codexGatewayRoute, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, c := range p.candidates {
		if !now.Before(c.route.expires) {
			delete(p.candidates, k)
		}
	}
	for k, until := range p.deadlines {
		if now.After(until.Add(10 * time.Minute)) {
			delete(p.deadlines, k)
		}
	}
	fingerprint := sha256.Sum256([]byte(route.cookie.Value))
	if until, ok := p.deadlines[fingerprint]; ok {
		if until.Before(route.expires) {
			route.expires = until
		}
	} else {
		if len(p.deadlines) >= 512 {
			return
		}
	}
	p.deadlines[fingerprint] = route.expires
	// An upstream shortening also constrains already published immutable snapshots.
	for key, c := range p.candidates {
		if c.route.cookie.Value == route.cookie.Value && route.expires.Before(c.route.expires) {
			c.route.expires = route.expires
			p.candidates[key] = c
		}
	}
	for _, identities := range p.identities {
		for _, entry := range identities {
			for i := range entry.passes {
				pass := &entry.passes[i]
				if pass.candidate.route.cookie.Value == route.cookie.Value && route.expires.Before(pass.candidate.route.expires) {
					pass.candidate.route.expires = route.expires
				}
			}
		}
	}
	if !now.Add(astraSeamlessMargin).Before(route.expires) {
		return
	}
	id := sha256.Sum256([]byte(route.cookie.Value + "\x00" + route.proxy + "\x00" + route.node.Identity))
	if _, ok := p.candidates[id]; ok {
		return
	}
	if len(p.candidates) >= 32 {
		var oldest [32]byte
		var expiry time.Time
		for k, c := range p.candidates {
			if expiry.IsZero() || c.route.expires.Before(expiry) {
				oldest = k
				expiry = c.route.expires
			}
		}
		delete(p.candidates, oldest) // validated passes own immutable copies
	}
	p.candidates[id] = astraSeamlessCandidate{id: id, source: source, route: route}
}

func seamlessIdentityKey(req *http.Request, proxy string, profile *tlsfingerprint.Profile) ([32]byte, [32]byte) {
	encoded, _ := json.Marshal(profile)
	parts := []string{req.URL.Path, proxy, string(encoded), "gpt-6-astra"}
	for _, name := range []string{"Authorization", "ChatGPT-Account-ID", "User-Agent", "Originator", "Version", "X-Codex-Turn-State"} {
		parts = append(parts, req.Header.Get(name))
	}
	auth := sha256.Sum256([]byte(req.Header.Get("Authorization") + "\x00" + req.Header.Get("ChatGPT-Account-ID")))
	return sha256.Sum256([]byte(strings.Join(parts, "\x00"))), auth
}

// Only the service's fresh account lookup may change the canonical credential
// generation. Business requests cannot resurrect a previously invalidated token.
func (p *astraSeamlessPool) identity(req *http.Request, proxy string, id int64, n int, profile *tlsfingerprint.Profile, canonical bool, now time.Time) (*astraSeamlessIdentity, error) {
	key, auth := seamlessIdentityKey(req, proxy, profile)
	p.mu.Lock()
	defer p.mu.Unlock()
	if canonical {
		delete(p.blocked, id)
		prior, knownCanonical := p.canonical[id]
		oldAuth, knownAuth := p.auth[id]
		if (knownCanonical && prior != key) || (knownAuth && oldAuth != auth) {
			for _, entry := range p.identities[id] {
				if entry.cancel != nil {
					entry.cancel()
				}
			}
			delete(p.identities, id)
		}
		p.auth[id] = auth
		p.canonical[id] = key
	} else {
		if reason := p.blocked[id]; reason != "" {
			return nil, errors.New(reason)
		}
		if current, ok := p.auth[id]; ok && current != auth {
			return nil, errors.New("astra_identity_changed")
		}
		if _, ok := p.auth[id]; !ok {
			p.auth[id] = auth
		}
	}
	if p.identities[id] == nil {
		p.identities[id] = map[[32]byte]*astraSeamlessIdentity{}
	}
	if entry := p.identities[id][key]; entry != nil {
		entry.seen = now
		return entry, nil
	}
	for k, entry := range p.identities[id] {
		if !entry.running && now.Sub(entry.seen) > 5*time.Minute && k != p.canonical[id] {
			delete(p.identities[id], k)
		}
	}
	if len(p.identities[id]) >= 4 {
		return nil, errors.New("astra_identity_capacity")
	}
	template := req.Clone(p.ctx)
	template.Body = nil
	template.GetBody = nil
	template.ContentLength = 0
	if profile != nil {
		raw, _ := json.Marshal(profile)
		profile = &tlsfingerprint.Profile{}
		_ = json.Unmarshal(raw, profile)
	}
	entry := &astraSeamlessIdentity{target: id, key: key, auth: auth, request: template, proxy: proxy, concurrency: n, profile: profile, seen: now, rejected: map[[32]byte]time.Time{}}
	p.identities[id][key] = entry
	return entry, nil
}

// Call with mu held. Expiry is checked at selection, not only on the 5s tick.
func (p *astraSeamlessPool) prune(entry *astraSeamlessIdentity, now time.Time) {
	entry.passes = slices.DeleteFunc(entry.passes, func(pass astraSeamlessPass) bool {
		return !now.Add(astraSeamlessMargin).Before(pass.candidate.route.expires)
	})
	for key, until := range entry.rejected {
		if !now.Before(until) {
			delete(entry.rejected, key)
		}
	}
}
func seamlessNeedsFill(entry *astraSeamlessIdentity, now time.Time, thresholds ...time.Duration) bool {
	threshold := astraSeamlessPrewarm
	if len(thresholds) > 0 {
		threshold = thresholds[0]
	}
	if len(entry.passes) < 2 {
		return true
	}
	latest := entry.passes[0].candidate.route.expires
	for _, pass := range entry.passes[1:] {
		if pass.candidate.route.expires.After(latest) {
			latest = pass.candidate.route.expires
		}
	}
	return !now.Add(threshold).Before(latest)
}

func (p *astraSeamlessPool) consume(source bool, id int64, now time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !source && now.Before(p.cooling[id]) {
		return errors.New("astra_request_budget")
	}
	p.requests = slices.DeleteFunc(p.requests, func(x astraSeamlessUsage) bool { return now.Sub(x.at) >= time.Minute })
	totalSource, account := 0, 0
	for _, x := range p.requests {
		if x.source {
			totalSource++
		}
		if x.source == source && x.account == id {
			account++
		}
	}
	if len(p.requests) >= 60 || (source && (totalSource >= 12 || account >= 4)) || (!source && account >= 24) {
		if !source && len(p.requests) > 0 {
			p.cooling[id] = p.requests[0].at.Add(time.Minute)
		}
		return errors.New("astra_request_budget")
	}
	p.requests = append(p.requests, astraSeamlessUsage{now, source, id})
	return nil
}

type astraSeamlessUpstream struct {
	service.HTTPUpstream
	p *astraSeamlessPool
}

func (u astraSeamlessUpstream) Do(req *http.Request, proxy string, id int64, n int) (*http.Response, error) {
	return u.DoWithTLS(req, proxy, id, n, nil)
}
func (u astraSeamlessUpstream) DoWithTLS(req *http.Request, proxy string, id int64, n int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	if err := u.p.consume(false, id, time.Now()); err != nil {
		return nil, err
	}
	resp, err := u.HTTPUpstream.DoWithTLS(req, proxy, id, n, profile)
	if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
		until := time.Now().Add(30 * time.Second)
		if seconds, e := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 32); e == nil && seconds >= 0 {
			until = time.Now().Add(time.Duration(seconds) * time.Second)
		} else if date, e := http.ParseTime(resp.Header.Get("Retry-After")); e == nil && date.After(time.Now()) {
			until = date
		}
		u.p.mu.Lock()
		u.p.cooling[id] = until
		u.p.mu.Unlock()
	}
	return resp, err
}

// Called with prepareMu held: acquire at most two donors per batch, rotating
// through configured sources instead of scheduling the entire source list.
func (s *astraRoutingUpstream) acquireSeamlessSources(ctx context.Context, pool *codexGatewayPinUpstream) error {
	if s.preparer == nil {
		return errors.New("source_preparer_unavailable")
	}
	p := pool.seamless
	p.mu.Lock()
	ids := []int64{}
	for range min(2, len(pool.config.SourceAccountIDs)) {
		ids = append(ids, pool.config.SourceAccountIDs[p.sourceCursor%len(pool.config.SourceAccountIDs)])
		p.sourceCursor++
	}
	p.mu.Unlock()
	s.preparing.Store(true)
	defer s.preparing.Store(false)
	var workers sync.WaitGroup
	outcomes := make(chan error, len(ids))
	for _, id := range ids {
		workers.Add(1)
		go func(id int64) {
			defer workers.Done()
			probe, cancel := context.WithTimeout(ctx, 45*time.Second)
			defer cancel()
			outcomes <- s.preparer(probe, id)
		}(id)
	}
	workers.Wait()
	close(outcomes)
	successes := 0
	var failure error
	for err := range outcomes {
		if err == nil {
			successes++
		} else if failure == nil || strings.Contains(err.Error(), "astra_request_budget") {
			failure = err
		}
	}
	if successes == 0 && failure != nil {
		return failure
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.current(ctx) != pool {
		return errors.New("configuration_changed")
	}
	return nil
}

func (s *astraRoutingUpstream) startSeamlessJob(pool *codexGatewayPinUpstream, entry *astraSeamlessIdentity, force bool) <-chan struct{} {
	p := pool.seamless
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prune(entry, time.Now())
	if entry.running {
		return entry.done
	}
	if p.ctx.Err() != nil || p.identities[entry.target][entry.key] != entry || time.Now().Before(p.cooling[entry.target]) || (!force && time.Now().Before(entry.next)) || (!force && !seamlessNeedsFill(entry, time.Now(), p.prewarm)) {
		return nil
	}
	if p.targetRunning[entry.target] {
		return nil
	}
	// Do not queue unlimited jobs. The next maintenance tick fairly retries.
	select {
	case p.workers <- struct{}{}:
	default:
		return nil
	}
	ctx, cancel := context.WithTimeout(p.ctx, 180*time.Second)
	p.targetRunning[entry.target] = true
	entry.running = true
	entry.done = make(chan struct{})
	entry.cancel = cancel
	entry.phase = "source"
	done := entry.done
	p.jobs.Add(1)
	go func() {
		defer p.jobs.Done()
		err := s.fillSeamless(ctx, pool, entry, force)
		cancel()
		p.mu.Lock()
		delete(p.targetRunning, entry.target)
		entry.running = false
		entry.cancel = nil
		entry.phase = "idle"
		if err != nil {
			entry.failures++
			entry.reason = err.Error()
			delay := min(30, 5<<min(entry.failures-1, 3))
			entry.next = time.Now().Add(time.Duration(delay)*time.Second + time.Duration(entry.target%5)*100*time.Millisecond)
		} else {
			entry.failures = 0
			entry.reason = ""
			entry.next = time.Now().Add(5 * time.Second)
		}
		close(done)
		p.mu.Unlock()
		<-p.workers
	}()
	return done
}

func (s *astraRoutingUpstream) fillSeamless(ctx context.Context, pool *codexGatewayPinUpstream, entry *astraSeamlessIdentity, force bool) error {
	p := pool.seamless
	attempted := map[[32]byte]bool{}
	batches := 0
	probes := 0
	for probes < 6 {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if s.current(ctx) != pool {
			return errors.New("configuration_changed")
		}
		p.mu.Lock()
		p.prune(entry, time.Now())
		if p.identities[entry.target][entry.key] != entry {
			p.mu.Unlock()
			return errors.New("astra_identity_changed")
		}
		if !force && !seamlessNeedsFill(entry, time.Now(), p.prewarm) {
			p.mu.Unlock()
			return nil
		}
		var candidates []astraSeamlessCandidate
		if force && len(entry.passes) > 0 {
			candidates = append(candidates, entry.passes[0].candidate)
		}
		for _, c := range p.candidates {
			if attempted[c.id] || time.Now().Before(entry.rejected[sha256.Sum256([]byte(c.route.cookie.Value))]) || !time.Now().Add(astraSeamlessMargin+5*time.Second).Before(c.route.expires) || !codexGatewayCookiePathMatches(entry.request.URL.Path, c.route.cookie.Path) {
				continue
			}
			if slices.ContainsFunc(entry.passes, func(pass astraSeamlessPass) bool { return pass.candidate.route.cookie.Value == c.route.cookie.Value }) {
				continue
			}
			if len(entry.passes) >= 2 {
				newest := entry.passes[0].candidate.route.expires
				for _, pass := range entry.passes[1:] {
					if pass.candidate.route.expires.After(newest) {
						newest = pass.candidate.route.expires
					}
				}
				// A full inventory needs more lifetime, not another equally old sample.
				if !c.route.expires.After(newest.Add(10 * time.Second)) {
					continue
				}
			}
			candidates = append(candidates, c)
		}
		if !force {
			sort.Slice(candidates, func(i, j int) bool {
				if len(entry.passes) > 0 {
					primary := entry.passes[0].candidate
					a, b := candidates[i].source != primary.source, candidates[j].source != primary.source
					if a != b {
						return a
					}
				}
				return candidates[i].route.expires.After(candidates[j].route.expires)
			})
		}
		p.mu.Unlock()
		if len(candidates) == 0 {
			if batches >= 3 {
				return errors.New("no_qualified_source_route")
			}
			batches++
			if err := s.prepareAstraGateway(ctx, true); err != nil {
				return err
			}
			continue
		}
		candidate := candidates[0]
		attempted[candidate.id] = true
		probes++
		p.mu.Lock()
		entry.phase = "target"
		p.mu.Unlock()
		proxy := entry.proxy
		if pool.config.IPAffinity {
			proxy = candidate.route.proxy
		}
		probeCtx, cancel := context.WithTimeout(ctx, min(90*time.Second, time.Until(candidate.route.expires)-astraSeamlessMargin))
		req := entry.request.Clone(probeCtx)
		replaceCodexGatewayCookie(req, &candidate.route.cookie)
		result := service.ProbeOpenAICodexStateRoute(probeCtx, astraSeamlessUpstream{s.delegate, p}, req, proxy, entry.target, entry.concurrency, entry.profile)
		cancel()
		if s.current(ctx) != pool {
			return errors.New("configuration_changed")
		}
		p.mu.Lock()
		if p.identities[entry.target][entry.key] != entry || p.ctx.Err() != nil {
			p.mu.Unlock()
			return errors.New("configuration_changed")
		}
		entry.failure = result.Failure
		entry.mintStatus = result.MintStatus
		entry.continueStatus = result.ContinueStatus
		entry.retries = result.RetryCount
		cookieFingerprint := sha256.Sum256([]byte(candidate.route.cookie.Value))
		if deadline, ok := p.deadlines[cookieFingerprint]; ok && deadline.Before(candidate.route.expires) {
			candidate.route.expires = deadline
		}
		passed := !time.Now().Before(entry.rejected[cookieFingerprint]) && result.Verdict == service.OpenAICodexStateHealthy && time.Now().Add(astraSeamlessMargin).Before(candidate.route.expires) && ctx.Err() == nil
		reason := "target_probe_failed"
		if result.Verdict == service.OpenAICodexStateDegraded {
			reason = "target_probe_degraded"
		}
		if result.Failure == "route_changed" {
			reason = "target_route_changed"
		}
		if passed {
			reason = "target_probe_passed"
			entry.passes = slices.DeleteFunc(entry.passes, func(pass astraSeamlessPass) bool { return pass.candidate.id == candidate.id })
			entry.passes = append(entry.passes, astraSeamlessPass{candidate, time.Now()})
			if len(entry.passes) > 2 {
				// Retain the active primary and the freshest backup.
				sort.Slice(entry.passes[1:], func(i, j int) bool {
					return entry.passes[i+1].candidate.route.expires.After(entry.passes[j+1].candidate.route.expires)
				})
				entry.passes = entry.passes[:2]
			}
		} else {
			entry.rejected[sha256.Sum256([]byte(candidate.route.cookie.Value))] = candidate.route.expires
			entry.passes = slices.DeleteFunc(entry.passes, func(pass astraSeamlessPass) bool { return pass.candidate.id == candidate.id })
		}
		entry.reason = reason
		limited := time.Now().Before(p.cooling[entry.target])
		if limited {
			entry.failure = service.OpenAICodexStateFailureRateLimited
		}
		p.mu.Unlock()
		pool.mu.Lock()
		pool.observeGatewayTarget(astraRoutingHost(candidate.route.cookie.Value), passed)
		pool.mu.Unlock()
		if pool.historyRecorder != nil {
			pool.historyRecorder(service.AstraGatewayHistoryRecord{Gateway: astraRoutingHost(candidate.route.cookie.Value), SourceAccountID: candidate.source, TargetAccountID: entry.target, LastSeen: time.Now(), LastReason: reason}, passed)
		}
		if force && passed {
			return nil
		}
		force = false
		if limited {
			return errors.New("astra_request_budget")
		}
		if result.Failure == service.OpenAICodexStateFailureAccountError || result.Failure == service.OpenAICodexStateFailureRateLimited {
			return errors.New(reason)
		}
	}
	return errors.New("target_probe_failed")
}

func (s *astraRoutingUpstream) seamlessTargetRoute(req *http.Request, proxy string, id int64, n int, profile *tlsfingerprint.Profile, pool *codexGatewayPinUpstream) (*http.Cookie, [32]byte, string, func(), error) {
	var zero [32]byte
	release := func() {}
	p := pool.seamless
	entry, err := p.identity(req, proxy, id, n, profile, false, time.Now())
	if err != nil {
		return nil, zero, proxy, release, err
	}
	force, _ := req.Context().Value(astraForceProbeKey{}).(bool)
	requested := time.Now()
	pick := func() (*http.Cookie, [32]byte, string, bool) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.prune(entry, time.Now())
		if p.ctx.Err() != nil || p.identities[id][entry.key] != entry || len(entry.passes) == 0 {
			return nil, zero, proxy, false
		}
		pass := entry.passes[0]
		if force {
			fresh := false
			for _, candidate := range entry.passes {
				if !candidate.checked.Before(requested) {
					pass = candidate
					fresh = true
					break
				}
			}
			if !fresh {
				return nil, zero, proxy, false
			}
		}
		c := pass.candidate
		cookie := c.route.cookie
		exit := proxy
		if pool.config.IPAffinity {
			exit = c.route.proxy
		}
		key := sha256.Sum256(append(append([]byte{}, entry.key[:]...), c.id[:]...))
		return &cookie, key, exit, true
	}
	if !force {
		if cookie, key, exit, ok := pick(); ok {
			return cookie, key, exit, release, nil
		}
	}
	done := s.startSeamlessJob(pool, entry, force)
	wait := 5 * time.Second
	if force {
		wait = 180 * time.Second
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	// Waiters subscribe to readiness, not just job completion: the first pass may
	// be usable while the same background job continues acquiring a backup.
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-req.Context().Done():
			return nil, zero, proxy, release, req.Context().Err()
		case <-timer.C:
			return nil, zero, proxy, release, errors.New("astra_route_warming")
		case <-done:
			if cookie, key, exit, ok := pick(); ok {
				return cookie, key, exit, release, nil
			}
			p.mu.Lock()
			reason := entry.reason
			p.mu.Unlock()
			if reason == "" {
				reason = "astra_route_warming"
			}
			return nil, zero, proxy, release, errors.New(reason)
		case <-tick.C:
			if done == nil {
				done = s.startSeamlessJob(pool, entry, force)
			}
			if !force {
				if cookie, key, exit, ok := pick(); ok {
					return cookie, key, exit, release, nil
				}
			}
			if p.ctx.Err() != nil {
				return nil, zero, proxy, release, errors.New("configuration_changed")
			}
		}
	}
}

// Nonblocking maintenance entry point called with freshly resolved account
// credentials. Known business identity profiles are maintained independently.
func (s *astraRoutingUpstream) MaintainAstraGatewayTarget(ctx context.Context, req *http.Request, proxy string, id int64, n int) {
	pool := s.current(ctx)
	if pool == nil || pool.seamless == nil {
		return
	}
	p := pool.seamless
	_, err := p.identity(req, proxy, id, n, nil, true, time.Now())
	if err != nil {
		return
	}
	p.mu.Lock()
	entries := []*astraSeamlessIdentity{}
	for _, entry := range p.identities[id] {
		if time.Since(entry.seen) < 5*time.Minute || entry.key == p.canonical[id] {
			entries = append(entries, entry)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return len(entries[i].passes) < len(entries[j].passes) })
	p.mu.Unlock()
	for _, entry := range entries {
		s.startSeamlessJob(pool, entry, false)
	}
}
func (s *astraRoutingUpstream) BlockAstraGatewayTarget(ctx context.Context, id int64, reason string) {
	pool := s.current(ctx)
	if pool == nil || pool.seamless == nil {
		return
	}
	p := pool.seamless
	p.mu.Lock()
	defer p.mu.Unlock()
	p.blocked[id] = reason
	for _, entry := range p.identities[id] {
		if entry.cancel != nil {
			entry.cancel()
		}
	}
	delete(p.identities, id)
}
func (s *astraRoutingUpstream) StopAstraSeamless() {
	s.mu.Lock()
	var p *astraSeamlessPool
	if s.pool != nil {
		p = s.pool.seamless
	}
	if p != nil {
		p.cancel()
	}
	s.mu.Unlock()
	if p != nil {
		p.mu.Lock()
		p.mu.Unlock()
		p.jobs.Wait()
	}
}

func (p *astraSeamlessPool) snapshot(result service.AstraGatewayRuntime, cfg config.CodexGatewayPinConfig, now time.Time) service.AstraGatewayRuntime {
	p.mu.Lock()
	defer p.mu.Unlock()
	readySources := map[int64]bool{}
	for _, id := range cfg.TargetAccountIDs {
		row := service.AstraRouteStatus{AccountID: id, State: "waiting", Reason: "target_not_verified"}
		if reason := p.blocked[id]; reason != "" {
			row.State = "blocked"
			row.Reason = reason
		}
		entry := p.identities[id][p.canonical[id]]
		if entry != nil {
			p.prune(entry, now)
			row.Maintenance = &service.AstraMaintenanceStatus{Phase: entry.phase, Reason: entry.reason, Failure: entry.failure, MintStatus: entry.mintStatus, ContinueStatus: entry.continueStatus, Retries: entry.retries, Running: entry.running}
			if !entry.next.IsZero() {
				next := entry.next
				if p.cooling[id].After(next) {
					next = p.cooling[id]
				}
				row.Maintenance.NextAttempt = &next
			}
			for i, pass := range entry.passes {
				c := pass.candidate
				role := "backup"
				if i == 0 {
					role = "primary"
				}
				row.Routes = append(row.Routes, service.AstraVerifiedRouteStatus{Role: role, SourceAccountID: c.source, Gateway: astraRoutingHost(c.route.cookie.Value), ExpiresAt: c.route.expires, CheckedAt: pass.checked, RemainingSeconds: max(0, int64(c.route.expires.Sub(now).Seconds()))})
				readySources[c.source] = true
				result.ReadyRoutes++
			}
			if len(entry.passes) > 0 {
				pass := entry.passes[0]
				expiry := pass.candidate.route.expires
				checked := pass.checked
				row.State = "ready"
				row.Reason = "target_probe_passed"
				row.Active = true
				row.ExpiresAt = &expiry
				row.CheckedAt = &checked
				row.RemainingSeconds = max(0, int64(expiry.Sub(now).Seconds()))
				row.Gateway = astraRoutingHost(pass.candidate.route.cookie.Value)
			} else if entry.reason != "" {
				row.Reason = entry.reason
			}
		}
		result.Targets = append(result.Targets, row)
	}
	for _, id := range cfg.SourceAccountIDs {
		row := service.AstraRouteStatus{AccountID: id, State: "waiting", Reason: "not_tested"}
		for _, c := range p.candidates {
			if c.source != id || !now.Before(c.route.expires) {
				continue
			}
			if row.ExpiresAt != nil && !c.route.expires.After(*row.ExpiresAt) {
				continue
			}
			expiry := c.route.expires
			row.ExpiresAt = &expiry
			row.RemainingSeconds = max(0, int64(expiry.Sub(now).Seconds()))
			row.Gateway = astraRoutingHost(c.route.cookie.Value)
			row.State = "candidate"
			row.Reason = "source_passed_pending_target"
		}
		if readySources[id] {
			row.State = "ready"
			row.Reason = "target_probe_passed"
		}
		result.Sources = append(result.Sources, row)
	}
	return result
}

// Response evidence revokes only the matching target/Cookie association. An
// already-started stream is returned as-is; business requests are never replayed.
func (p *astraSeamlessPool) observeBusiness(req *http.Request, proxy string, id int64, profile *tlsfingerprint.Profile, cookie *http.Cookie, resp *http.Response) {
	if resp == nil || cookie == nil {
		return
	}
	changed := false
	for _, got := range resp.Cookies() {
		if got.Name == "__oailb" && (got.Value != cookie.Value || got.MaxAge < 0 || (!got.Expires.IsZero() && !time.Now().Before(got.Expires))) {
			changed = true
		}
	}
	if !changed && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		return
	}
	key, _ := seamlessIdentityKey(req, proxy, profile)
	p.mu.Lock()
	defer p.mu.Unlock()
	entry := p.identities[id][key]
	if entry == nil {
		return
	}
	if changed {
		for _, pass := range entry.passes {
			if pass.candidate.route.cookie.Value == cookie.Value {
				entry.rejected[sha256.Sum256([]byte(cookie.Value))] = pass.candidate.route.expires
			}
		}
		entry.passes = slices.DeleteFunc(entry.passes, func(pass astraSeamlessPass) bool { return pass.candidate.route.cookie.Value == cookie.Value })
		entry.reason = "target_route_changed"
		entry.failure = "route_changed"
	} else {
		entry.passes = nil
		if entry.cancel != nil {
			entry.cancel()
		}
		entry.reason = "account_unavailable"
		entry.failure = service.OpenAICodexStateFailureAccountError
		p.cooling[id] = time.Now().Add(30 * time.Second)
	}
}
