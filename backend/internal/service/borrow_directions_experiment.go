//go:build astra_experiment

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/google/uuid"
)

// Diagnostics only: no routes are published and all normal admission guards remain.
type BorrowDirectionsTrigger struct {
	RouteID   string                 `json:"route_id"`
	At        time.Time              `json:"at"`
	Candidate BorrowCandidateDetails `json:"candidate"`
}
type BorrowDirectionsInput struct {
	Trigger       *BorrowDirectionsTrigger `json:"trigger,omitempty"`
	Mode          string                   `json:"mode"`
	TargetID      int64                    `json:"target_id"`
	SourceIDs     []int64                  `json:"source_ids"`
	SharedFirst   bool                     `json:"shared_first"`
	Deadline      *time.Time               `json:"deadline,omitempty"`
	ParallelFirst bool                     `json:"parallel_first"`
	FreshFirst    bool                     `json:"fresh_first"`
}
type BorrowDirectionAcquisitionSpan struct {
	CandidateID     string    `json:"candidate_id"`
	SourceAccountID int64     `json:"source_account_id"`
	StartedAt       time.Time `json:"started_at"`
	FinishedAt      time.Time `json:"finished_at"`
}
type BorrowDirectionsResult struct {
	Trigger             *BorrowDirectionsTrigger         `json:"trigger,omitempty"`
	TargetLeasesReadyAt *time.Time                       `json:"target_leases_ready_at,omitempty"`
	Acquisitions        []BorrowDirectionAcquisitionSpan `json:"acquisitions,omitempty"`
	ID                  string                           `json:"id"`
	Mode                string                           `json:"mode"`
	StartedAt           time.Time                        `json:"started_at"`
	FinishedAt          time.Time                        `json:"finished_at"`
	Outcome             string                           `json:"outcome"`
	Arms                []BorrowDirectionArm             `json:"arms"`
}
type BorrowDirectionArm struct {
	EffectiveExpiresAt *time.Time                   `json:"effective_expires_at,omitempty"`
	Name               string                       `json:"name"`
	TargetID           int64                        `json:"target_id"`
	Candidate          BorrowCandidateDetails       `json:"candidate"`
	Result             *OpenAICodexStateProbeResult `json:"result,omitempty"`
	Shots              []*BorrowDirectionShot       `json:"shots"`
	Business           *AstraExitBusinessResult     `json:"business,omitempty"`
	Failure            string                       `json:"failure,omitempty"`
}
type BorrowDirectionShot struct {
	Phase       string                `json:"phase"`
	SessionHash string                `json:"session_hash"`
	BodyHash    string                `json:"body_hash"`
	CookieHash  string                `json:"cookie_hash"`
	TicketHash  string                `json:"ticket_hash"`
	Status      int                   `json:"status"`
	StartedAt   time.Time             `json:"started_at"`
	FinishedAt  time.Time             `json:"finished_at"`
	HeadersMS   int64                 `json:"headers_ms"`
	DurationMS  int64                 `json:"duration_ms"`
	Text        string                `json:"text,omitempty"`
	Stream      AstraDiagnosticStream `json:"stream"`
	SSE         string                `json:"sse,omitempty"`
}
type directionTrace struct {
	HTTPUpstream
	session  string
	shots    []*BorrowDirectionShot
	minted   string
	business bool
}

func (u *directionTrace) DoWithTLS(req *http.Request, proxy string, id int64, n int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	// Clone preserves the original probe's authentication, cookie, body and Close flag.
	req = req.Clone(req.Context())
	if u.session != "" {
		req.Header.Set("session_id", u.session)
	}
	obs := &BorrowDirectionShot{Phase: "mint", SessionHash: AstraExperimentHash(req.Header.Get("session_id")), CookieHash: AstraExperimentHash(req.Header.Get("Cookie")), StartedAt: time.Now()}
	if req.Header.Get(openAICodexTurnStateHeader) != "" {
		obs.Phase = "continue"
	}
	if u.business {
		obs.Phase = "independent_business"
	}
	if req.GetBody != nil {
		b, e := req.GetBody()
		if e == nil {
			data, _ := io.ReadAll(b)
			b.Close()
			obs.BodyHash = AstraExperimentHash(string(data))
		}
	}
	u.shots = append(u.shots, obs)
	resp, err := u.HTTPUpstream.DoWithTLS(req, proxy, id, n, profile)
	obs.HeadersMS = time.Since(obs.StartedAt).Milliseconds()
	finish := func(data []byte) {
		obs.FinishedAt = time.Now()
		obs.DurationMS = time.Since(obs.StartedAt).Milliseconds()
		obs.Text = astraBusinessControlText(data)
		obs.Stream = astraDiagnosticStream(data)
		if obs.Phase == "independent_business" {
			obs.SSE = string(data)
		}
	}
	if resp != nil {
		obs.Status = resp.StatusCode
		obs.TicketHash = AstraExperimentHash(resp.Header.Get(openAICodexTurnStateHeader))
		if obs.Phase == "mint" {
			u.minted = resp.Header.Get(openAICodexTurnStateHeader)
		}
		if resp.Body != nil {
			resp.Body = &directionBody{ReadCloser: resp.Body, finish: finish}
			return resp, err
		}
	}
	finish(nil)
	return resp, err
}

type directionBody struct {
	io.ReadCloser
	data   bytes.Buffer
	finish func([]byte)
	once   sync.Once
}

func (b *directionBody) Read(p []byte) (int, error) {
	n, e := b.ReadCloser.Read(p)
	if b.data.Len() < openAICodexStateProbeMaxBody {
		b.data.Write(p[:min(n, openAICodexStateProbeMaxBody-b.data.Len())])
	}
	return n, e
}
func (b *directionBody) Close() error {
	e := b.ReadCloser.Close()
	b.once.Do(func() { b.finish(b.data.Bytes()) })
	return e
}

type directionTarget struct {
	account *Account
	target  BorrowTarget
	acq     BorrowAcquisition
	ctx     context.Context
	base    *http.Request
	proxy   string
	profile *tlsfingerprint.Profile
	model   string
}
type directionCandidate struct {
	target  *directionTarget
	source  BorrowSource
	details BorrowCandidateDetails
	cookie  *http.Cookie
	expires time.Time
}

func (x *BorrowPairedExitExperiment) RunDirections(parent context.Context, in BorrowDirectionsInput) (out BorrowDirectionsResult) {
	s := x.service
	out = BorrowDirectionsResult{ID: "directions-" + uuid.NewString(), Mode: in.Mode, StartedAt: time.Now(), Arms: []BorrowDirectionArm{}}
	defer func() { out.FinishedAt = time.Now() }()
	if ((in.Mode == "window" || in.Mode == "gateway") && len(in.SourceIDs) != 1) || len(in.SourceIDs) == 0 || len(in.SourceIDs) > 4 || (in.TargetID != 63 && in.TargetID != 64) {
		out.Outcome = "invalid_input"
		return
	}
	switch in.Mode {
	case "session", "serial", "parallel", "retain", "window", "gateway", "rolling", "triggered":
	default:
		out.Outcome = "invalid_mode"
		return
	}
	budget := 165 * time.Second
	if in.Mode == "rolling" || in.Mode == "triggered" {
		if len(in.SourceIDs) != 2 || in.SourceIDs[0] == in.SourceIDs[1] {
			out.Outcome = "invalid_rolling_sources"
			return
		}
		budget = 245 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.WithValue(parent, borrowInternalKey{}, true), budget)
	defer cancel()
	if in.Mode == "triggered" {
		if in.Trigger == nil || in.Trigger.RouteID == "" || time.Since(in.Trigger.At) < 0 || time.Since(in.Trigger.At) > 15*time.Second || in.Trigger.Candidate.SourceAccountID != in.SourceIDs[0] {
			out.Outcome = "invalid_or_stale_trigger"
			return
		}
		out.Trigger = in.Trigger
		ctx = WithBorrowDirectionsReadyWindow(ctx, out.ID, in.Trigger.At.Add(160*time.Second))
	}
	if in.Deadline != nil {
		var stop context.CancelFunc
		ctx, stop = context.WithDeadline(ctx, *in.Deadline)
		defer stop()
	}
	settings, err := s.repo.Settings(ctx)
	if err != nil || !settings.Running {
		out.Outcome = "borrow_paused"
		return
	}
	minimumRemaining := 120
	if settings.MinRouteRemainingSeconds != nil {
		minimumRemaining = *settings.MinRouteRemainingSeconds
	}
	targets := map[int64]*directionTarget{}
	var leaseDeadline time.Time
	// Hold both local targets because they share one upstream identity. Normal
	// scheduler leases and cooldowns arbitrate; no direct database mutations.
	waitUntil := time.Now().Add(75 * time.Second)
	if in.Mode == "triggered" {
		waitUntil = minBorrowTime(waitUntil, in.Trigger.At.Add(15*time.Second))
	}
	for _, id := range []int64{63, 64} {
		t, e := s.repo.Target(ctx, id)
		if e != nil || !t.Enabled {
			out.Outcome = "target_unavailable"
			return
		}
		a, e := s.normalBorrowAccount(ctx, id, OpenAICodexStateProbeDefaultModel)
		if e != nil || !diagnosticTokenReady(a) {
			out.Outcome = "target_health_or_token_unavailable"
			return
		}
		_, identity, e := s.targetEnvironment(ctx, a)
		if e != nil {
			out.Outcome = "target_environment_unavailable"
			return
		}
		acq := BorrowAcquisition{TargetID: id, Revision: t.Revision, Identity: identity, Lease: out.ID}
		tc := WithBorrowAcquisition(ctx, acq)
		for {
			ok, e := s.repo.AcquireJob(tc, "target", id, out.ID)
			if e != nil {
				out.Outcome = "target_admission_failed"
				return
			}
			if ok {
				if leaseDeadline.IsZero() {
					leaseDeadline = time.Now().Add(170 * time.Second)
				}
				break
			}
			if time.Now().After(waitUntil) || tc.Err() != nil {
				out.Outcome = "targets_busy_or_waiting"
				return
			}
			select {
			case <-tc.Done():
			case <-time.After(200 * time.Millisecond):
			}
		}
		defer func(id int64) {
			cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer stop()
			_ = s.repo.FinishJob(cleanup, "target", id, out.ID, "diagnostic_directions_finished", 0)
		}(id)
		base, proxy, _, e := s.owner.astraTargetTemplate(tc, id)
		if e != nil {
			out.Outcome = "target_template_unavailable"
			return
		}
		var profile *tlsfingerprint.Profile
		if s.owner.tlsFPProfileService != nil {
			profile = s.owner.tlsFPProfileService.ResolveTLSProfile(a)
		}
		if id == in.TargetID && x.ExpectedOriginal != nil {
			actual := borrowEnvironment(base, proxy, profile)
			expected := x.ExpectedOriginal
			if actual.IdentityHash != expected.IdentityHash || actual.ProxyHash != expected.ProxyHash || actual.ProfileHash != expected.ProfileHash {
				out.Outcome = "production_environment_mismatch"
				return
			}
		}
		targets[id] = &directionTarget{a, t, acq, tc, base, proxy, profile, normalizeOpenAIModelForUpstream(a, a.GetMappedModel(OpenAICodexStateProbeDefaultModel))}
	}
	leasesReady := time.Now()
	out.TargetLeasesReadyAt = &leasesReady
	acquireObserved := func(ctx context.Context, source BorrowSource, settings BorrowSettings, details *BorrowCandidateDetails) (*http.Cookie, time.Time, error) {
		span := BorrowDirectionAcquisitionSpan{CandidateID: details.CandidateID, SourceAccountID: source.AccountID, StartedAt: time.Now()}
		cookie, expires, err := s.acquireSourceObserved(ctx, source, settings, details)
		span.FinishedAt = time.Now()
		out.Acquisitions = append(out.Acquisitions, span)
		return cookie, expires, err
	}
	sources, e := s.repo.Sources(ctx)
	if e != nil {
		out.Outcome = "source_store_unavailable"
		return
	}
	candidates := []directionCandidate{}
	for i, sourceID := range in.SourceIDs {
		if (in.Mode == "rolling" || in.Mode == "triggered") && i > 0 {
			break
		}
		tid := in.TargetID
		if in.Mode == "parallel" || in.Mode == "serial" {
			tid = []int64{63, 64}[i%2]
		}
		t := targets[tid]
		var src *BorrowSource
		for j := range sources {
			if sources[j].AccountID == sourceID {
				src = &sources[j]
				break
			}
		}
		if src == nil || !src.Enabled {
			out.Arms = append(out.Arms, BorrowDirectionArm{Name: in.Mode, TargetID: tid, Failure: "source_unavailable", Candidate: BorrowCandidateDetails{SourceAccountID: sourceID}})
			continue
		}
		// List APIs intentionally omit binding secrets. Read the source snapshot
		// used by acquisition so the window's drift guard compares real bindings.
		loaded, loadErr := s.repo.Source(t.ctx, src.ID)
		if loadErr != nil || loaded == nil {
			out.Outcome = "source_snapshot_unavailable"
			return
		}
		src = loaded
		a, e := s.normalSourceAccount(t.ctx, *src)
		if e != nil || !diagnosticTokenReady(a) {
			out.Arms = append(out.Arms, BorrowDirectionArm{Name: in.Mode, TargetID: tid, Failure: "source_health_or_token_unavailable", Candidate: BorrowCandidateDetails{SourceAccountID: sourceID}})
			continue
		}
		if in.Mode == "triggered" {
			route, e := s.repo.Route(t.ctx, t.target.AccountID, t.acq.Identity, in.Trigger.RouteID)
			if e != nil || route == nil || route.SourceID != src.ID || route.CookieHash != in.Trigger.Candidate.CookieHash {
				out.Outcome = "trigger_route_unavailable"
				return
			}
			d := in.Trigger.Candidate
			d.ExpiresAt = &route.ExpiresAt
			candidates = append(candidates, directionCandidate{t, *src, d, &route.Secret.Cookie, route.ExpiresAt})
			continue
		}
		d := BorrowCandidateDetails{CandidateID: uuid.NewString(), BatchID: out.ID, TargetRevision: t.target.Revision, TargetIdentityHash: t.acq.Identity, SourceAccountID: sourceID}
		cookie, expires, e := acquireObserved(t.ctx, *src, settings, &d)
		if e != nil || cookie == nil {
			out.Arms = append(out.Arms, BorrowDirectionArm{Name: in.Mode, TargetID: tid, Failure: "source_acquisition_failed", Candidate: d})
			continue
		}
		d.ExpiresAt = &expires
		candidates = append(candidates, directionCandidate{t, *src, d, cookie, expires})
	}
	probe := func(c directionCandidate, name string, shared bool, businessCheck bool) BorrowDirectionArm {
		t := c.target
		arm := BorrowDirectionArm{Name: name, TargetID: t.target.AccountID, Candidate: c.details}
		guard := func(check context.Context) error {
			if in.Mode == "window" || in.Mode == "gateway" || in.Mode == "rolling" || in.Mode == "triggered" {
				current, e := s.repo.Source(check, c.source.ID)
				if e != nil || !directionSameSource(current, &c.source) {
					return errors.New("source_environment_changed")
				}
			}
			if e := s.repo.CheckAcquisition(check, t.acq); e != nil {
				return e
			}
			a, e := s.normalBorrowAccount(check, t.target.AccountID, t.model)
			if e != nil || !diagnosticTokenReady(a) || a.GetOpenAIAccessToken() != t.account.GetOpenAIAccessToken() || borrowAccountIdentity(a) != borrowAccountIdentity(t.account) {
				return ErrBorrowUnavailable
			}
			if !time.Now().Before(c.expires) {
				return errors.New("candidate_expired")
			}
			if (in.Mode == "gateway" || in.Mode == "rolling" || in.Mode == "triggered") && time.Until(c.expires) <= time.Duration(minimumRemaining)*time.Second {
				return errors.New("candidate_below_business_remaining_threshold")
			}
			return nil
		}
		if e := guard(t.ctx); e != nil {
			arm.Failure = "guard_stopped"
			return arm
		}
		pc, stop := context.WithDeadline(t.ctx, minBorrowTime(c.expires, time.Now().Add(65*time.Second)))
		defer stop()
		pc = context.WithValue(pc, borrowBeforeSendKey{}, guard)
		q := BorrowRequest{Kind: "probe", TargetID: t.target.AccountID, SourceID: c.source.ID, BatchID: out.ID, Revision: t.target.Revision, CandidateID: c.details.CandidateID + ":" + name}
		pc = context.WithValue(pc, borrowProbeRequestKey{}, q)
		pc = context.WithValue(pc, openAICodexShotObserverKey{}, s.borrowShotObserver(t.account, t.model, t.account.GetOpenAIAccessToken()))
		req := t.base.Clone(pc)
		replaceBorrowCookie(req, c.cookie)
		trace := &directionTrace{HTTPUpstream: s.owner.httpUpstream}
		if shared {
			trace.session = uuid.NewString()
		}
		arm.Result = ProbeOpenAICodexStateRoute(pc, trace, req, t.proxy, t.target.AccountID, 2, t.profile, t.model)
		arm.Result.Detail = ""
		effectiveExpires := c.expires
		if !arm.Result.RouteCookieDeadline.IsZero() {
			effectiveExpires = minBorrowTime(effectiveExpires, arm.Result.RouteCookieDeadline)
		}
		if expiry := t.account.GetOpenAITokenExpiresAt(); expiry != nil {
			effectiveExpires = minBorrowTime(effectiveExpires, *expiry)
		}
		arm.EffectiveExpiresAt = &effectiveExpires
		arm.Candidate.Target = borrowEnvironment(req, t.proxy, t.profile)
		arm.Candidate.ProbeVerdict = string(arm.Result.Verdict)
		arm.Candidate.ProbeFailure = arm.Result.Failure
		arm.Candidate.MintStatus = arm.Result.MintStatus
		arm.Candidate.ContinueStatus = arm.Result.ContinueStatus
		age := time.Until(c.expires).Milliseconds()
		arm.Candidate.RemainingMS = &age
		s.candidateEvent(pc, BorrowEvent{TargetID: t.target.AccountID, SourceID: c.source.ID, Stage: "diagnostic", Reason: "directions_" + name}, &arm.Candidate, c.expires)
		if reason, wait := borrowProbeUpstreamWait(arm.Result); wait > 0 {
			clean, done := context.WithTimeout(context.WithoutCancel(pc), 3*time.Second)
			_ = s.repo.RecordProbeOutcome(clean, t.target.AccountID, out.ID, reason, wait)
			done()
		}
		if businessCheck && arm.Result.Verdict == OpenAICodexStateHealthy && arm.Result.Failure == "" && trace.minted != "" && guard(pc) == nil {
			// A genuinely fresh session must still use the minted ticket and route. Keep
			// this bounded diagnostic business check separate from probe success counts.
			trace.session = ""
			trace.business = true
			q.Kind = "business"
			q.Phase = "diagnostic_independent_business"
			bc := context.WithValue(pc, borrowProbeRequestKey{}, q)
			prompt := "计算 37 × 19，只输出结果。"
			body, _ := json.Marshal(map[string]any{"model": t.model, "instructions": "Answer accurately.", "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": prompt}}}}, "stream": true, "store": false})
			routeChanged := false
			shot, e := fireOpenAICodexStateShotRequest(bc, req.Header, t.model, trace.minted, arm.Result.RouteCookie, func(r *http.Request) (*http.Response, error) {
				r.Body.Close()
				r.Body = io.NopCloser(bytes.NewReader(body))
				r.ContentLength = int64(len(body))
				r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
				resp, e := trace.DoWithTLS(r, t.proxy, t.target.AccountID, 2, t.profile)
				if resp != nil {
					for _, got := range resp.Cookies() {
						if got.Name == "__oailb" && (got.Value != c.cookie.Value || got.MaxAge < 0 || (!got.Expires.IsZero() && !time.Now().Before(got.Expires))) {
							routeChanged = true
						}
					}
				}
				return resp, e
			})
			check := &OpenAICodexStateProbeResult{}
			complete := check.shotUsable(bc, "", shot, e)
			arm.Business = &AstraExitBusinessResult{Complete: complete, NewTicket: shot.state != "" && shot.state != trace.minted, RouteChanged: routeChanged, Failure: check.Failure}
		}
		arm.Shots = trace.shots
		return arm
	}
	if in.Mode == "rolling" || in.Mode == "triggered" {
		if len(candidates) != 1 {
			out.Outcome = "seed_unavailable"
			return
		}
		seed := candidates[0]
		var initialTime time.Time
		if in.Mode == "triggered" {
			initialTime = in.Trigger.At
		} else {
			initial := probe(seed, "seed", false, false)
			out.Arms = append(out.Arms, initial)
			if initial.Result == nil || initial.Result.Verdict != OpenAICodexStateHealthy || initial.Result.Failure != "" {
				out.Outcome = "seed_not_passed"
				return
			}
			initialTime = initial.Result.FinishedAt
		}
		end := minBorrowTime(leaseDeadline, initialTime.Add(145*time.Second))
		cutoff := seed.expires.Add(-time.Duration(minimumRemaining) * time.Second)
		// Require at least 20 seconds of observation beyond the seed's admission
		// cutoff. Never extend a production lease or cookie for this experiment.
		if !end.After(cutoff.Add(20 * time.Second)) {
			out.Outcome = "insufficient_lease_window"
			return
		}
		wc, stop := context.WithDeadline(seed.target.ctx, end)
		defer stop()
		wt := *seed.target
		wt.ctx = wc
		baselines := map[string]*BorrowSource{"retain": &seed.source}
		for _, src := range sources {
			if src.AccountID == in.SourceIDs[1] {
				loaded, err := s.repo.Source(wc, src.ID)
				if err == nil && loaded != nil && loaded.Enabled {
					baselines["alternate"] = loaded
				}
			}
		}
		if baselines["alternate"] == nil {
			out.Outcome = "alternate_unavailable"
			return
		}
		seen := map[string]bool{seed.details.CookieHash: true}
		for pair := 0; pair < 12; pair++ {
			// Both groups receive equal candidate slots and alternate the early
			// position. Failed acquisitions consume their slot, never disappear.
			if wait := time.Until(initialTime.Add(time.Duration(pair) * 12 * time.Second)); wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-wc.Done():
					timer.Stop()
					out.Outcome = "rolling_deadline"
					return
				case <-timer.C:
				}
			}
			order := []string{"retain", "alternate"}
			if (pair%2 == 1) != in.FreshFirst {
				order[0], order[1] = order[1], order[0]
			}
			for _, mode := range order {
				if wc.Err() != nil {
					out.Outcome = "rolling_deadline"
					return
				}
				name := fmt.Sprintf("rolling_%d_%s", pair+1, mode)
				base := baselines[mode]
				current, e := s.repo.Source(wc, base.ID)
				if e != nil || !directionSameSource(current, base) {
					out.Outcome = "source_environment_changed"
					return
				}
				d := BorrowCandidateDetails{CandidateID: uuid.NewString(), BatchID: out.ID, TargetRevision: wt.target.Revision, TargetIdentityHash: wt.acq.Identity, SourceAccountID: base.AccountID}
				cookie, expires, e := acquireObserved(wc, *current, settings, &d)
				if e != nil || cookie == nil {
					out.Arms = append(out.Arms, BorrowDirectionArm{Name: name, TargetID: wt.target.AccountID, Candidate: d, Failure: "source_acquisition_failed"})
					continue
				}
				d.ExpiresAt = &expires
				if in.Mode == "triggered" && mode == "retain" && (d.Source.ProxyHash != seed.details.Source.ProxyHash || d.Source.ProfileHash != seed.details.Source.ProfileHash || d.Source.IdentityHash != seed.details.Source.IdentityHash) {
					out.Arms = append(out.Arms, BorrowDirectionArm{Name: name, TargetID: wt.target.AccountID, Candidate: d, Failure: "trigger_source_environment_changed"})
					out.Outcome = "trigger_source_environment_changed"
					return
				}
				if seen[d.CookieHash] {
					out.Arms = append(out.Arms, BorrowDirectionArm{Name: name, TargetID: wt.target.AccountID, Candidate: d, Failure: "duplicate_cookie"})
					continue
				}
				seen[d.CookieHash] = true
				arm := probe(directionCandidate{&wt, *current, d, cookie, expires}, name, false, false)
				out.Arms = append(out.Arms, arm)
				if arm.Result == nil {
					out.Outcome = "rolling_guard_stopped"
					return
				}
				if _, wait := borrowProbeUpstreamWait(arm.Result); wait > 0 {
					out.Outcome = "rolling_upstream_wait"
					return
				}
			}
		}
		out.Outcome = "rolling_budget_complete"
		return
	}
	if in.Mode == "window" || in.Mode == "gateway" {
		if len(candidates) != 1 {
			out.Outcome = "seed_unavailable"
			return
		}
		seed := candidates[0]
		initial := probe(seed, "seed", false, false)
		out.Arms = append(out.Arms, initial)
		if initial.Result == nil || initial.Result.Verdict != OpenAICodexStateHealthy || initial.Result.Failure != "" {
			out.Outcome = "seed_not_passed"
			return
		}
		wc, stop := context.WithDeadline(seed.target.ctx, initial.Result.FinishedAt.Add(90*time.Second))
		defer stop()
		wt := *seed.target
		wt.ctx = wc
		seen := map[string]bool{seed.details.CookieHash: true}
		if in.Mode == "gateway" {
			// Reuse only the successful routing cookie. Every arm runs the normal
			// two-shot probe, with a fresh mint and independent session IDs. This
			// does not recycle the seed's turn-state ticket or publish a route.
			for pair, order := range directionGatewayOrder(in.FreshFirst) {
				for _, mode := range order {
					if wc.Err() != nil {
						out.Outcome = "window_deadline"
						return
					}
					name := fmt.Sprintf("gateway_%d_%s", pair+1, mode)
					c := directionCandidate{&wt, seed.source, seed.details, seed.cookie, seed.expires}
					if mode == "fresh" {
						current, e := s.repo.Source(wc, seed.source.ID)
						if e != nil || !directionSameSource(current, &seed.source) {
							out.Outcome = "source_environment_changed"
							return
						}
						d := BorrowCandidateDetails{CandidateID: uuid.NewString(), BatchID: out.ID, TargetRevision: wt.target.Revision, TargetIdentityHash: wt.acq.Identity, SourceAccountID: seed.source.AccountID}
						cookie, expires, e := acquireObserved(wc, *current, settings, &d)
						if e != nil || cookie == nil {
							out.Arms = append(out.Arms, BorrowDirectionArm{Name: name, TargetID: wt.target.AccountID, Candidate: d, Failure: "source_acquisition_failed"})
							out.Outcome = "incomplete_gateway_pair"
							return
						}
						d.ExpiresAt = &expires
						if d.Source.ProxyHash != seed.details.Source.ProxyHash || d.Source.ProfileHash != seed.details.Source.ProfileHash || d.Source.IdentityHash != seed.details.Source.IdentityHash {
							out.Outcome = "source_environment_changed"
							return
						}
						if seen[d.CookieHash] {
							out.Outcome = "duplicate_fresh_cookie"
							return
						}
						seen[d.CookieHash] = true
						c = directionCandidate{&wt, *current, d, cookie, expires}
					}
					arm := probe(c, name, false, false)
					out.Arms = append(out.Arms, arm)
					if arm.Result == nil || arm.Result.Failure != "" {
						out.Outcome = "gateway_probe_interrupted"
						return
					}
				}
			}
			out.Outcome = "gateway_window_complete"
			return
		}
		for block, mode := range directionWindowOrder(in.ParallelFirst) {
			if wc.Err() != nil {
				out.Outcome = "window_deadline"
				return
			}
			name := fmt.Sprintf("window_%d_%s", block+1, mode)
			fresh := []directionCandidate{}
			for j := 0; j < 2; j++ {
				current, e := s.repo.Source(wc, seed.source.ID)
				if e != nil || !directionSameSource(current, &seed.source) {
					out.Outcome = "source_environment_changed"
					return
				}
				d := BorrowCandidateDetails{CandidateID: uuid.NewString(), BatchID: out.ID, TargetRevision: wt.target.Revision, TargetIdentityHash: wt.acq.Identity, SourceAccountID: seed.source.AccountID}
				cookie, expires, e := acquireObserved(wc, *current, settings, &d)
				if e != nil || cookie == nil {
					out.Arms = append(out.Arms, BorrowDirectionArm{Name: name, TargetID: wt.target.AccountID, Candidate: d, Failure: "source_acquisition_failed"})
					continue
				}
				d.ExpiresAt = &expires
				if d.Source.ProxyHash != seed.details.Source.ProxyHash || d.Source.ProfileHash != seed.details.Source.ProfileHash || d.Source.IdentityHash != seed.details.Source.IdentityHash {
					out.Outcome = "source_environment_changed"
					return
				}
				if seen[d.CookieHash] {
					out.Arms = append(out.Arms, BorrowDirectionArm{Name: name, TargetID: wt.target.AccountID, Candidate: d, Failure: "duplicate_cookie"})
					continue
				}
				seen[d.CookieHash] = true
				fresh = append(fresh, directionCandidate{&wt, *current, d, cookie, expires})
			}
			// A parallel arm needs two candidates. Do not relabel a single actual
			// request as a concurrency comparison when source acquisition failed.
			if len(fresh) != 2 {
				out.Outcome = "incomplete_window_block"
				return
			}
			arms := make([]BorrowDirectionArm, 2)
			if mode == "parallel" {
				var wg sync.WaitGroup
				for i, c := range fresh {
					wg.Add(1)
					go func(i int, c directionCandidate) { defer wg.Done(); arms[i] = probe(c, name, false, false) }(i, c)
				}
				wg.Wait()
			} else {
				for i, c := range fresh {
					arms[i] = probe(c, name, false, false)
				}
			}
			out.Arms = append(out.Arms, arms...)
			if wc.Err() != nil {
				out.Outcome = "window_deadline"
				return
			}
		}
		out.Outcome = "window_complete"
		return
	}
	if in.Mode == "session" {
		if len(candidates) > 0 {
			names := []string{"independent_session", "shared_session"}
			if in.SharedFirst {
				names = []string{"shared_session", "independent_session"}
			}
			for _, name := range names {
				out.Arms = append(out.Arms, probe(candidates[0], name, name == "shared_session", true))
			}
		}
	} else if in.Mode == "parallel" {
		arms := make([]BorrowDirectionArm, len(candidates))
		var wg sync.WaitGroup
		for i, c := range candidates {
			wg.Add(1)
			go func(i int, c directionCandidate) { defer wg.Done(); arms[i] = probe(c, in.Mode, false, false) }(i, c)
		}
		wg.Wait()
		out.Arms = append(out.Arms, arms...)
	} else {
		for _, c := range candidates {
			out.Arms = append(out.Arms, probe(c, in.Mode, false, false))
		}
	}
	out.Outcome = "completed"
	return
}

// The balanced crossover limits monotonic time/order confounding within a
// short-lived positive window, without changing target policy or session IDs.
func directionWindowOrder(parallelFirst bool) []string {
	if parallelFirst {
		return []string{"parallel", "serial", "serial", "parallel"}
	}
	return []string{"serial", "parallel", "parallel", "serial"}
}

func directionGatewayOrder(freshFirst bool) [][2]string {
	first, second := "pinned", "fresh"
	if freshFirst {
		first, second = second, first
	}
	return [][2]string{{first, second}, {second, first}, {first, second}, {second, first}}
}
func directionSameSource(a, b *BorrowSource) bool {
	return a != nil && b != nil && a.Enabled && b.Enabled && a.Model == b.Model && a.Secret.Fingerprint != nil && b.Secret.Fingerprint != nil && a.AccountID == b.AccountID && a.PoolEntryID == b.PoolEntryID && len(a.Secret.Proxies) == 1 && len(b.Secret.Proxies) == 1 && a.Secret.Proxies[0] == b.Secret.Proxies[0] && a.Secret.DeviceID == b.Secret.DeviceID && sameBorrowFingerprint(a.Secret.Fingerprint, b.Secret.Fingerprint)
}

// Only the diagnostic overlay consumes this context marker. Production builds
// contain neither these functions nor an alternate readiness/schedule branch.
type borrowDirectionsReadyKey struct{}
type borrowDirectionsReadyScope struct {
	batch string
	until time.Time
}

func WithBorrowDirectionsReadyWindow(ctx context.Context, batch string, until time.Time) context.Context {
	return context.WithValue(ctx, borrowDirectionsReadyKey{}, borrowDirectionsReadyScope{batch, until})
}
func BorrowDirectionsAllowsReady(ctx context.Context, a BorrowAcquisition) bool {
	scope, ok := ctx.Value(borrowDirectionsReadyKey{}).(borrowDirectionsReadyScope)
	bound, boundOK := BorrowAcquisitionFromContext(ctx)
	return ok && boundOK && scope.batch != "" && time.Now().Before(scope.until) && (a.TargetID == 63 || a.TargetID == 64) && bound.TargetID == a.TargetID && bound.Lease == scope.batch && (a.Lease == "" || a.Lease == scope.batch)
}
