package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/google/uuid"
)

const AccountQualityMonitorPrompt = CandyPrompt + "\n最终结果只输出整数"
const accountQualityLeaseDuration = 90 * time.Second
const accountQualityRenewInterval = 20 * time.Second

type accountQualityScopeContextKey struct{}

// AccountActivityScope isolates observations without using short-lived OAuth
// tokens. API keys are hashed before being used in either persistence or Redis.
func AccountActivityScope(account *Account) string {
	return AccountActivityScopeWithCredentialAccount(account, account)
}

// AccountActivityScopeWithCredentialAccount includes a shadow's resolved parent
// identity, so replacing the parent's login invalidates the child's observations.
// An unresolved or invalid shadow cannot acquire an observation scope.
func AccountActivityScopeWithCredentialAccount(account, credentialAccount *Account) string {
	if account == nil {
		return ""
	}
	if account.IsCredentialShadow() {
		if credentialAccount == nil || credentialAccount.ID != *account.ParentAccountID || credentialAccount.IsCredentialShadow() || !credentialAccount.IsOpenAIOAuth() {
			return ""
		}
	} else {
		credentialAccount = account
	}
	identity := ""
	if credentialAccount.IsOpenAIOAuthLike() {
		identity = codexAccountIdentityNamespace(credentialAccount)
		if identity == "" {
			identity = "email:" + strings.ToLower(strings.TrimSpace(credentialAccount.GetCredential("email")))
		}
	} else {
		keyHash := sha256.Sum256([]byte(credentialAccount.GetOpenAIApiKey()))
		identity = strings.TrimRight(strings.TrimSpace(credentialAccount.GetOpenAIBaseURL()), "/") + ":" + hex.EncodeToString(keyHash[:])
	}
	parts := []any{"account-activity-v1", account.ID, account.Platform, account.Type, account.ParentAccountID, identity}
	mode := account.EffectiveExecutionMode()
	if account.IsVMKernel() {
		mode = AccountExecutionModeDockerKernel
	}
	// Preserve existing normal-account observations while separating execution
	// modes. Empty legacy modes and explicit "normal" are the same scope.
	if mode != AccountExecutionModeNormal {
		parts = append(parts, mode)
	}
	raw, _ := json.Marshal(parts)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

var accountQualityFinalAnswer = regexp.MustCompile(`^(?:(?:最终答案|最终结论|答案)\s*[:：是为]?\s*|(?:最少|至少)(?:需要|取出|摸出)?\s*)?(\d+)\s*(?:个(?:糖果)?)?[。.!！]?$`)

func judgeAccountQualityAnswer(text string) (string, string) {
	clean := strings.TrimSpace(strings.NewReplacer("**", "", "__", "").Replace(text))
	lines := strings.Split(clean, "\n")
	match := accountQualityFinalAnswer.FindStringSubmatch(strings.TrimSpace(lines[len(lines)-1]))
	if len(match) == 0 {
		return "unknown", ""
	}
	if strings.TrimLeft(match[1], "0") == "21" {
		return "passed", match[1]
	}
	return "failed", match[1]
}

func qualityMonitorText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return text
}

type AccountQualityMonitorService struct {
	repo          AccountQualityMonitorRepository
	accounts      AccountRepository
	tests         *AccountTestService
	ctx           context.Context
	cancel        context.CancelFunc
	mu            sync.Mutex
	stopped       bool
	active        map[int64]context.CancelFunc
	slots         chan struct{}
	wg            sync.WaitGroup
	now           func() time.Time
	execute       func(context.Context, int64, string) (*ScheduledTestResult, error)
	renewInterval time.Duration
}

func NewAccountQualityMonitorService(repo AccountQualityMonitorRepository, accounts AccountRepository, tests *AccountTestService) *AccountQualityMonitorService {
	ctx, cancel := context.WithCancel(context.Background())
	s := &AccountQualityMonitorService{repo: repo, accounts: accounts, tests: tests, ctx: ctx, cancel: cancel,
		active: make(map[int64]context.CancelFunc), slots: make(chan struct{}, 2), now: time.Now, renewInterval: accountQualityRenewInterval}
	s.execute = func(ctx context.Context, id int64, model string) (*ScheduledTestResult, error) {
		cfg := &PelicanTestConfig{QuestionKind: "candy", TestChannel: "account", Prompt: AccountQualityMonitorPrompt,
			ParallelCount: 1, Quality: &QualityPolicy{Action: QualityActionObserveOnly, ExpectedAnswer: "21"}}
		return tests.runAccountTextTest(ctx, id, model, cfg, AccountQualityMonitorPrompt, true)
	}
	return s
}

func defaultAccountQualityConfig(account *Account) AccountQualityMonitorConfig {
	return AccountQualityMonitorConfig{Scope: AccountActivityScope(account), Model: "gpt-6.1-sol", IntervalMinutes: 5}
}

// IsAccountQualityTextModel checks the actual mapped target while allowing
// custom text models exposed by OpenAI-compatible providers.
func IsAccountQualityTextModel(account *Account, model string) bool {
	if account == nil {
		return false
	}
	target := strings.ToLower(codexProviderQualifiedModelID(account.GetMappedModel(strings.TrimSpace(model))))
	if target == "" || isCodexDedicatedMediaModel(target) {
		return false
	}
	for _, family := range []string{"dall-e", "whisper", "tts", "text-embedding", "text-moderation", "omni-moderation", "sora"} {
		if target == family || strings.HasPrefix(target, family+"-") {
			return false
		}
	}
	if strings.HasPrefix(target, "gpt-") {
		for _, part := range strings.Split(target, "-") {
			if part == "audio" || part == "realtime" || part == "transcribe" || part == "tts" {
				return false
			}
		}
	}
	return true
}

func (s *AccountQualityMonitorService) account(ctx context.Context, id int64) (*Account, string, error) {
	account, err := s.accounts.GetByID(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if account == nil {
		return nil, "", ErrAccountNotFound
	}
	if account.Platform != PlatformOpenAI {
		return nil, "", infraerrors.BadRequest("ACCOUNT_QUALITY_PLATFORM", "quality monitoring requires an OpenAI account")
	}
	// The authorized child may use a parent outside the observer's visible groups.
	// Read its identity internally; only the digest is returned to the caller.
	internalCtx := context.WithValue(ctx, observerScopeKey{}, struct{}{})
	credentialAccount, err := resolveCredentialAccount(internalCtx, s.accounts, account)
	if err != nil {
		return nil, "", ErrAccountQualityUnavailable
	}
	scope := AccountActivityScopeWithCredentialAccount(account, credentialAccount)
	if scope == "" {
		return nil, "", ErrAccountQualityUnavailable
	}
	return account, scope, nil
}

func accountQualityPauseReason(account *Account, now time.Time) string {
	if !account.IsActive() {
		return "帐号已停用或不可用"
	}
	if account.AutoPauseOnExpired && account.ExpiresAt != nil && !now.Before(*account.ExpiresAt) {
		return "帐号已过期"
	}
	for _, window := range []string{"5h", "7d"} {
		if value, known := resolveOpenAIQuotaUtilization(account.Extra, window, now); known && value >= 1 {
			return strings.ToUpper(window) + " 额度受限"
		}
	}
	if account.Type == AccountTypeAPIKey && account.IsQuotaExceeded() {
		return "帐号额度受限"
	}
	if account.RateLimitResetAt != nil && now.Before(*account.RateLimitResetAt) {
		return "帐号临时限流"
	}
	if account.OverloadUntil != nil && now.Before(*account.OverloadUntil) {
		return "上游暂时过载"
	}
	if account.TempUnschedulableUntil != nil && now.Before(*account.TempUnschedulableUntil) {
		return "帐号暂时不可用"
	}
	return ""
}

func (s *AccountQualityMonitorService) view(account *Account, scope string, record *AccountQualityMonitorRecord, details bool) *AccountQualityMonitorView {
	result := &AccountQualityMonitorView{AccountID: account.ID, AccountQualityMonitorConfig: defaultAccountQualityConfig(account), State: "untested"}
	result.Scope = scope
	now := s.now()
	result.PauseReason = accountQualityPauseReason(account, now)
	if scope == "" {
		result.State, result.PauseReason = "paused", "母帐号暂时不可用"
		if record != nil {
			result.Enabled = record.Enabled
		}
		return result
	}
	if record == nil || record.Scope != result.Scope {
		return result
	}
	result.AccountQualityMonitorConfig = record.AccountQualityMonitorConfig
	if result.PauseReason == "" && !IsAccountQualityTextModel(account, record.Model) {
		result.PauseReason = "当前模型不支持文本质量检测"
	}
	clone := func(value *AccountQualityMonitorResult) *AccountQualityMonitorResult {
		if value == nil {
			return nil
		}
		copy := *value
		if !details {
			copy.Text = ""
		}
		return &copy
	}
	result.LatestAttempt, result.LastValid = clone(record.LatestAttempt), clone(record.LastValid)
	result.NextDueAt = record.NextDueAt
	if result.LatestAttempt != nil {
		result.State = result.LatestAttempt.State
	}
	switch {
	case record.RunID != "" && record.LeaseUntil != nil && record.LeaseUntil.After(now):
		result.State = "running"
	case result.Enabled && result.PauseReason != "":
		result.State, result.NextDueAt = "paused", nil
	case result.Enabled && result.NextDueAt != nil && !result.NextDueAt.After(now):
		result.State = "queued"
	}
	if !result.Enabled {
		result.NextDueAt = nil
	}
	return result
}

func (s *AccountQualityMonitorService) Get(ctx context.Context, id int64, details bool) (*AccountQualityMonitorView, error) {
	account, scope, err := s.account(ctx, id)
	if err != nil {
		return nil, err
	}
	record, err := s.repo.Get(ctx, id, details)
	if err != nil {
		return nil, err
	}
	return s.view(account, scope, record, details), nil
}

func (s *AccountQualityMonitorService) Batch(ctx context.Context, ids []int64) (map[int64]*AccountQualityMonitorView, error) {
	accounts, err := s.accounts.GetByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	return s.BatchForAccounts(ctx, accounts)
}

func (s *AccountQualityMonitorService) BatchForAccounts(ctx context.Context, accounts []*Account) (map[int64]*AccountQualityMonitorView, error) {
	scopes, err := s.scopesForAccounts(ctx, accounts)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(accounts))
	for _, a := range accounts {
		if a != nil && a.Platform == PlatformOpenAI {
			ids = append(ids, a.ID)
		}
	}
	records, err := s.repo.Batch(ctx, ids)
	if err != nil {
		return nil, err
	}
	views := make(map[int64]*AccountQualityMonitorView, len(ids))
	for _, a := range accounts {
		if a != nil && a.Platform == PlatformOpenAI {
			views[a.ID] = s.view(a, scopes[a.ID], records[a.ID], false)
		}
	}
	return views, nil
}

func (s *AccountQualityMonitorService) scopesForAccounts(ctx context.Context, accounts []*Account) (map[int64]string, error) {
	parents := make(map[int64]*Account)
	var parentIDs []int64
	for _, account := range accounts {
		if account != nil && account.IsCredentialShadow() {
			if _, seen := parents[*account.ParentAccountID]; !seen {
				parents[*account.ParentAccountID] = nil
				parentIDs = append(parentIDs, *account.ParentAccountID)
			}
		}
	}
	if len(parentIDs) > 0 {
		internalCtx := context.WithValue(ctx, observerScopeKey{}, struct{}{})
		resolved, err := s.accounts.GetByIDs(internalCtx, parentIDs)
		if err != nil {
			return nil, err
		}
		for _, parent := range resolved {
			if parent != nil {
				parents[parent.ID] = parent
			}
		}
	}
	scopes := make(map[int64]string, len(accounts))
	for _, account := range accounts {
		if account == nil {
			continue
		}
		credentialAccount := account
		if account.IsCredentialShadow() {
			credentialAccount = parents[*account.ParentAccountID]
		}
		scopes[account.ID] = AccountActivityScopeWithCredentialAccount(account, credentialAccount)
	}
	return scopes, nil
}

func (s *AccountQualityMonitorService) Save(ctx context.Context, id int64, config AccountQualityMonitorConfig) (*AccountQualityMonitorView, error) {
	account, scope, err := s.account(ctx, id)
	if err != nil {
		return nil, err
	}
	if config.Scope != scope {
		return nil, ErrAccountQualityScopeChanged
	}
	config.Model = strings.TrimSpace(config.Model)
	if config.IntervalMinutes < 1 || config.IntervalMinutes > 1440 || config.Model == "" || len(config.Model) > 160 {
		return nil, infraerrors.BadRequest("ACCOUNT_QUALITY_CONFIG", "model is required and interval must be 1–1440 minutes")
	}
	if !IsAccountQualityTextModel(account, config.Model) {
		return nil, ErrAccountQualityTextModel
	}
	// Disabling or adjusting an existing model never needs an upstream catalog call.
	old, err := s.repo.Get(ctx, id, false)
	if err != nil {
		return nil, err
	}
	if s.tests != nil && (old == nil || old.Scope != config.Scope || old.Model != config.Model) {
		models, lookupErr := s.tests.FetchOpenAIAccountModels(ctx, account)
		if lookupErr != nil {
			return nil, infraerrors.New(503, "ACCOUNT_QUALITY_MODELS_UNAVAILABLE", "cannot load account models")
		}
		supported := false
		for _, model := range models {
			if model.ID == config.Model {
				supported = true
				break
			}
		}
		if !supported {
			return nil, infraerrors.BadRequest("ACCOUNT_QUALITY_MODEL_UNSUPPORTED", "selected model is not available for this account")
		}
	}
	record, err := s.repo.Save(ctx, id, config, s.now())
	if err != nil {
		return nil, err
	}
	return s.view(account, scope, record, true), nil
}

func (s *AccountQualityMonitorService) Start(ctx context.Context, id int64, scope string) (*AccountQualityMonitorView, error) {
	account, currentScope, err := s.account(ctx, id)
	if err != nil {
		return nil, err
	}
	if scope != currentScope {
		return nil, ErrAccountQualityScopeChanged
	}
	record, err := s.repo.Get(ctx, id, false)
	if err != nil {
		return nil, err
	}
	if record == nil || record.Scope != scope {
		config := defaultAccountQualityConfig(account)
		config.Scope = scope
		record, err = s.repo.Ensure(ctx, id, config, s.now())
		if err != nil {
			return nil, err
		}
	}
	if !IsAccountQualityTextModel(account, record.Model) {
		return nil, ErrAccountQualityTextModel
	}
	if reason := accountQualityPauseReason(account, s.now()); reason != "" {
		return nil, fmt.Errorf("%w: %s", ErrAccountQualityUnavailable, reason)
	}
	if err = s.dispatch(ctx, record, false); err != nil {
		return nil, err
	}
	return s.Get(ctx, id, true)
}

func (s *AccountQualityMonitorService) dispatch(ctx context.Context, record *AccountQualityMonitorRecord, automatic bool) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return ErrAccountQualityStopped
	}
	if _, running := s.active[record.AccountID]; running {
		s.mu.Unlock()
		return nil
	}
	select {
	case s.slots <- struct{}{}:
	default:
		s.mu.Unlock()
		return ErrAccountQualityBusy
	}
	runCtx, cancel := context.WithCancel(s.ctx)
	s.active[record.AccountID] = cancel
	s.wg.Add(1)
	s.mu.Unlock()
	releaseLocal := func() {
		cancel()
		s.mu.Lock()
		delete(s.active, record.AccountID)
		s.mu.Unlock()
		<-s.slots
		s.wg.Done()
	}
	runID := uuid.NewString()
	now := s.now()
	claimed, err := s.repo.Claim(ctx, record, runID, now, now.Add(accountQualityLeaseDuration), automatic)
	if err != nil || !claimed {
		releaseLocal()
		return err
	}
	go func() { defer releaseLocal(); s.run(runCtx, record, runID) }()
	return nil
}

func (s *AccountQualityMonitorService) run(ctx context.Context, record *AccountQualityMonitorRecord, runID string) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer func() {
		_ = recover() // A provider adapter panic must release this task's ownership.
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		_ = s.repo.Release(cleanup, record.AccountID, runID)
	}()
	heartbeatDone := make(chan struct{})
	heartbeatStopped := make(chan struct{})
	go func() {
		defer close(heartbeatStopped)
		ticker := time.NewTicker(s.renewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-heartbeatDone:
				return
			case <-ticker.C:
				renewCtx, stop := context.WithTimeout(ctx, 3*time.Second)
				retained, err := s.repo.Renew(renewCtx, record.AccountID, runID, s.now().Add(accountQualityLeaseDuration))
				stop()
				if err != nil || !retained {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { close(heartbeatDone); <-heartbeatStopped }()
	account, scope, err := s.account(ctx, record.AccountID)
	if err != nil || scope != record.Scope || accountQualityPauseReason(account, s.now()) != "" || !IsAccountQualityTextModel(account, record.Model) {
		return
	}
	started := s.now()
	result, err := s.execute(context.WithValue(ctx, accountQualityScopeContextKey{}, record.Scope), record.AccountID, record.Model)
	if ctx.Err() != nil {
		return
	}
	attempt := &AccountQualityMonitorResult{State: "error", Model: record.Model, CheckedAt: s.now(), DurationMS: s.now().Sub(started).Milliseconds()}
	if err != nil || result == nil || result.Status != "success" {
		attempt.Error = "检测请求失败或响应未完整结束"
	} else {
		attempt.State, attempt.Answer = judgeAccountQualityAnswer(result.ResponseText)
		attempt.Text = qualityMonitorText(result.ResponseText, 12000)
	}
	_, scope, err = s.account(ctx, record.AccountID)
	if err != nil || scope != record.Scope {
		return
	}
	saveCtx, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	_, _ = s.repo.Finish(saveCtx, record, runID, attempt, s.now())
}

func (s *AccountQualityMonitorService) RunDue(ctx context.Context, now time.Time) {
	s.mu.Lock()
	available := cap(s.slots) - len(s.slots)
	stopped := s.stopped
	s.mu.Unlock()
	if stopped || available == 0 {
		return
	}
	records, err := s.repo.ListDue(ctx, now, 64)
	if err != nil {
		return
	}
	ids := make([]int64, 0, len(records))
	for _, r := range records {
		ids = append(ids, r.AccountID)
	}
	accounts, err := s.accounts.GetByIDs(ctx, ids)
	if err != nil {
		return
	}
	scopes, err := s.scopesForAccounts(ctx, accounts)
	if err != nil {
		return
	}
	byID := make(map[int64]*Account, len(accounts))
	for _, a := range accounts {
		byID[a.ID] = a
	}
	for _, record := range records {
		account := byID[record.AccountID]
		if account == nil || account.Platform != PlatformOpenAI || scopes[record.AccountID] == "" {
			_ = s.repo.Postpone(ctx, record, now.Add(time.Minute))
			continue
		}
		if record.Scope != scopes[record.AccountID] {
			config := defaultAccountQualityConfig(account)
			config.Scope = scopes[record.AccountID]
			_, _ = s.repo.Ensure(ctx, account.ID, config, now)
			continue
		}
		if accountQualityPauseReason(account, now) != "" || !IsAccountQualityTextModel(account, record.Model) {
			_ = s.repo.Postpone(ctx, record, now.Add(time.Minute))
			continue
		}
		if err = s.dispatch(ctx, record, true); err == ErrAccountQualityBusy || err == ErrAccountQualityStopped {
			return
		}
	}
}

func (s *AccountQualityMonitorService) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.stopped = true
	s.cancel()
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
}
