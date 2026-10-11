//go:build astra_experiment

package service

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/google/uuid"
)

// This bridge is absent from production builds. It acquires real source/target
// leases and request budgets, uses normal account health handling, and never
// publishes an experimental route or changes account/rotation configuration.
type BorrowPairedExitExperiment struct {
	service          *TicketBorrowService
	capture          *pairedExitCapture
	ExpectedOriginal *BorrowEnvironment
}
type BorrowPairedExitArm struct {
	Name        string                       `json:"name"`
	Environment BorrowEnvironment            `json:"environment"`
	RemainingMS int64                        `json:"remaining_ms"`
	Result      *OpenAICodexStateProbeResult `json:"result"`
}
type BorrowPairedExitResult struct {
	ID              string                  `json:"id"`
	TargetID        int64                   `json:"target_id"`
	SourceAccountID int64                   `json:"source_account_id"`
	StartedAt       time.Time               `json:"started_at"`
	FinishedAt      time.Time               `json:"finished_at"`
	Outcome         string                  `json:"outcome"`
	Calibration     string                  `json:"calibration,omitempty"`
	Candidate       *BorrowCandidateDetails `json:"candidate,omitempty"`
	Arms            []BorrowPairedExitArm   `json:"arms"`
}
type pairedExitCapture struct {
	HTTPUpstream
	sourceID int64
	header   http.Header
	proxy    string
	profile  *tlsfingerprint.Profile
}

func (u *pairedExitCapture) DoWithTLS(req *http.Request, proxy string, id int64, n int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	if id == u.sourceID {
		u.header = req.Header.Clone()
		u.proxy = proxy
		u.profile = profile
	}
	return u.HTTPUpstream.DoWithTLS(req, proxy, id, n, profile)
}
func NewBorrowPairedExitExperiment(repo TicketBorrowRepository, accounts AccountRepository, up HTTPUpstream, cfg *config.Config, tls *TLSFingerprintProfileService, settings *SettingService) *BorrowPairedExitExperiment {
	capture := &pairedExitCapture{HTTPUpstream: up}
	rates := NewRateLimitService(accounts, nil, cfg, nil, nil)
	rates.SetSettingService(settings)
	SetCodexCanonicalUserAgentResolver(func() string { return settings.GetOpenAICodexCanonicalUserAgent(context.Background()) })
	gateway := &OpenAIGatewayService{accountRepo: accounts, cfg: cfg, httpUpstream: capture, rateLimitService: rates, settingService: settings}
	rates.SetAccountRuntimeBlocker(gateway)
	owner := &AccountTestService{accountRepo: accounts, httpUpstream: capture, cfg: cfg, tlsFPProfileService: tls, openaiGatewayService: gateway}
	s := NewTicketBorrowService(repo, owner, nil)
	if setter, ok := up.(interface{ SetTicketBorrowService(*TicketBorrowService) }); ok {
		setter.SetTicketBorrowService(s)
	}
	return &BorrowPairedExitExperiment{service: s, capture: capture}
}
func diagnosticTokenReady(a *Account) bool {
	if a == nil || a.GetOpenAIAccessToken() == "" || a.IsOpenAIAgentIdentity() {
		return false
	}
	expiry := a.GetOpenAITokenExpiresAt()
	// Refresh belongs to the running service's TokenProvider, not this CLI.
	return expiry == nil || time.Until(*expiry) > 3*time.Minute
}
func pairedExitTemplate(base *http.Request, header http.Header) *http.Request {
	req := base.Clone(base.Context())
	// Copy only the source's transport identity. Target authentication and
	// account headers must never be copied from the source account.
	for _, key := range []string{"User-Agent", "Originator", "Version", "Accept-Language", "X-Codex-Installation-ID", "X-Codex-Window-ID"} {
		req.Header.Del(key)
		for _, v := range header.Values(key) {
			req.Header.Add(key, v)
		}
	}
	return req
}
func (x *BorrowPairedExitExperiment) Run(ctx context.Context, targetID, sourceAccount int64, matchedFirst bool) (out BorrowPairedExitResult) {
	s := x.service
	out = BorrowPairedExitResult{ID: "paired-exit-" + uuid.NewString(), TargetID: targetID, SourceAccountID: sourceAccount, StartedAt: time.Now(), Outcome: "not_started", Arms: []BorrowPairedExitArm{}}
	defer func() { out.FinishedAt = time.Now() }()
	ctx, cancel := context.WithTimeout(context.WithValue(ctx, borrowInternalKey{}, true), 240*time.Second)
	defer cancel()
	settings, err := s.repo.Settings(ctx)
	if err != nil || !settings.Running {
		out.Outcome = "borrow_paused"
		return
	}
	target, err := s.repo.Target(ctx, targetID)
	if err != nil || !target.Enabled {
		out.Outcome = "target_unavailable"
		return
	}
	account, err := s.normalBorrowAccount(ctx, targetID, OpenAICodexStateProbeDefaultModel)
	if err != nil {
		out.Outcome = "target_health_" + borrowHealthReason(err)
		return
	}
	if !diagnosticTokenReady(account) {
		out.Outcome = "target_token_unavailable"
		return
	}
	_, identity, err := s.targetEnvironment(ctx, account)
	if err != nil {
		out.Outcome = "target_environment_unavailable"
		return
	}
	acquisition := BorrowAcquisition{TargetID: targetID, Revision: target.Revision, Identity: identity, Lease: out.ID}
	ctx = WithBorrowAcquisition(ctx, acquisition)
	ok, err := s.repo.AcquireJob(ctx, "target", targetID, out.ID)
	leaseWaitUntil := time.Now().Add(90 * time.Second)
	for err == nil && !ok && time.Now().Before(leaseWaitUntil) && ctx.Err() == nil {
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
		ok, err = s.repo.AcquireJob(ctx, "target", targetID, out.ID)
	}
	if err != nil || !ok {
		out.Outcome = "target_busy_or_waiting"
		return
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer stop()
		_ = s.repo.FinishJob(cleanup, "target", targetID, out.ID, "diagnostic_paired_exit_finished", 0)
	}()
	sources, err := s.repo.Sources(ctx)
	if err != nil {
		out.Outcome = "source_store_unavailable"
		return
	}
	var source *BorrowSource
	for i := range sources {
		if sources[i].AccountID == sourceAccount {
			source = &sources[i]
			break
		}
	}
	if source == nil || !source.Enabled {
		out.Outcome = "source_unavailable"
		return
	}
	sa, err := s.normalSourceAccount(ctx, *source)
	if err != nil || !diagnosticTokenReady(sa) {
		out.Outcome = "source_health_or_token_unavailable"
		return
	}
	x.capture.sourceID = sourceAccount
	x.capture.header = nil
	details := &BorrowCandidateDetails{CandidateID: uuid.NewString(), BatchID: out.ID, TargetRevision: target.Revision, TargetIdentityHash: identity}
	out.Candidate = details
	cookie, expires, err := s.acquireSourceObserved(ctx, *source, settings, details)
	if err != nil || cookie == nil {
		out.Outcome = "source_acquisition_failed"
		return
	}
	details.ExpiresAt = &expires
	if x.capture.header == nil || x.capture.profile == nil || x.capture.proxy == "" {
		out.Outcome = "source_capture_missing"
		return
	}
	base, baseProxy, _, err := s.owner.astraTargetTemplate(ctx, targetID)
	if err != nil {
		out.Outcome = "target_template_unavailable"
		return
	}
	var baseProfile *tlsfingerprint.Profile
	if s.owner.tlsFPProfileService != nil {
		baseProfile = s.owner.tlsFPProfileService.ResolveTLSProfile(account)
	}
	actual := borrowEnvironment(base, baseProxy, baseProfile)
	expected := x.ExpectedOriginal
	if expected == nil || actual.IdentityHash != expected.IdentityHash || actual.ProxyHash != expected.ProxyHash || actual.ProfileHash != expected.ProfileHash {
		out.Outcome = "production_environment_mismatch"
		return
	}
	out.Calibration = "production_environment_matched"
	model := normalizeOpenAIModelForUpstream(account, account.GetMappedModel(OpenAICodexStateProbeDefaultModel))
	token := account.GetOpenAIAccessToken()
	guard := func(check context.Context) error {
		if err := s.repo.CheckAcquisition(check, acquisition); err != nil {
			return err
		}
		current, e := s.normalBorrowAccount(check, targetID, model)
		if e != nil || !diagnosticTokenReady(current) || current.GetOpenAIAccessToken() != token || borrowAccountIdentity(current) != borrowAccountIdentity(account) {
			return ErrBorrowUnavailable
		}
		if !time.Now().Before(expires) {
			return errors.New("candidate_expired")
		}
		return nil
	}
	order := []string{"target_original", "source_matched"}
	if matchedFirst {
		order = []string{"source_matched", "target_original"}
	}
	for _, name := range order {
		if err = guard(ctx); err != nil {
			out.Outcome = "guard_stopped"
			return
		}
		if time.Until(expires) < 30*time.Second {
			out.Outcome = "candidate_lifetime_insufficient"
			return
		}
		req, proxy, profile := base.Clone(ctx), baseProxy, baseProfile
		if name == "source_matched" {
			req = pairedExitTemplate(base, x.capture.header)
			proxy = x.capture.proxy
			profile = x.capture.profile
		}
		probeCtx, stop := context.WithDeadline(ctx, minBorrowTime(expires, time.Now().Add(65*time.Second)))
		probeCtx = context.WithValue(probeCtx, borrowBeforeSendKey{}, guard)
		probeCtx = context.WithValue(probeCtx, borrowProbeRequestKey{}, BorrowRequest{Kind: "probe", TargetID: targetID, SourceID: source.ID, BatchID: out.ID, Revision: target.Revision, CandidateID: details.CandidateID + ":" + name})
		probeCtx = context.WithValue(probeCtx, openAICodexShotObserverKey{}, s.borrowShotObserver(account, model, token))
		req = req.WithContext(probeCtx)
		replaceBorrowCookie(req, cookie)
		env := borrowEnvironment(req, proxy, profile)
		env.TLSConfigHash, env.TLSStructureHash = BorrowTLSFingerprintHashes(profile)
		arm := BorrowPairedExitArm{Name: name, Environment: env, RemainingMS: time.Until(expires).Milliseconds()}
		arm.Result = ProbeOpenAICodexStateRoute(probeCtx, s.owner.httpUpstream, req, proxy, targetID, 1, profile, model)
		arm.Result.Detail = ""
		stop()
		out.Arms = append(out.Arms, arm)
		d := *details
		d.Target = env
		d.ProbeVerdict = string(arm.Result.Verdict)
		d.ProbeFailure = arm.Result.Failure
		d.MintStatus = arm.Result.MintStatus
		d.ContinueStatus = arm.Result.ContinueStatus
		s.candidateEvent(ctx, BorrowEvent{TargetID: targetID, SourceID: source.ID, Stage: "diagnostic", Reason: "paired_exit_" + name}, &d, expires)
		if reason, wait := borrowProbeUpstreamWait(arm.Result); wait > 0 {
			cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			_ = s.repo.RecordProbeOutcome(cleanup, targetID, out.ID, reason, wait)
			done()
			out.Outcome = "upstream_wait_stopped"
			return
		}
	}
	out.Outcome = "paired_complete"
	return
}
