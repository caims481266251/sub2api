package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAccountQualityFinalAnswerAndIdentityScope(t *testing.T) {
	for _, tc := range []struct{ answer, state, value string }{
		{"推导可能是 29\n最终答案：21", "passed", "21"},
		{"21\n29", "failed", "29"}, {"**21**", "passed", "21"},
		{"答案为 21 或 29", "unknown", ""}, {"", "unknown", ""}, {"21.0", "unknown", ""},
	} {
		state, value := judgeAccountQualityAnswer(tc.answer)
		require.Equal(t, tc.state, state)
		require.Equal(t, tc.value, value)
	}
	a := &Account{ID: 4, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "account-a", "access_token": "old"}}
	before := AccountActivityScope(a)
	a.Credentials["access_token"] = "new"
	require.Equal(t, before, AccountActivityScope(a))
	a.Credentials["chatgpt_account_id"] = "account-b"
	require.NotEqual(t, before, AccountActivityScope(a))
	a.Type = AccountTypeAPIKey
	a.Credentials = map[string]any{"api_key": "canary-secret", "base_url": "https://api.openai.com/"}
	before = AccountActivityScope(a)
	require.NotContains(t, before, "canary-secret")
	a.Credentials["base_url"] = "https://api.openai.com"
	require.Equal(t, before, AccountActivityScope(a))
	a.Credentials["api_key"] = "changed"
	require.NotEqual(t, before, AccountActivityScope(a))
}

func TestAccountQualityScopeSeparatesExecutionModes(t *testing.T) {
	account := &Account{ID: 41, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "same-upstream", "access_token": "old"}}
	normal := AccountActivityScope(account)
	account.ExecutionMode = AccountExecutionModeNormal
	require.Equal(t, normal, AccountActivityScope(account), "legacy empty mode remains normal")
	account.ExecutionMode = AccountExecutionModeDockerKernel
	kernel := AccountActivityScope(account)
	require.NotEqual(t, normal, kernel, "normal and Docker execution must not share observations")
	account.Credentials["access_token"] = "refreshed"
	require.Equal(t, kernel, AccountActivityScope(account))
	account.ExecutionMode = "vm_kernel"
	require.Equal(t, kernel, AccountActivityScope(account), "legacy VM naming identifies the same Docker execution mode")
}

func TestAccountQualityStrictProtocolCompletion(t *testing.T) {
	cases := []struct {
		name, protocol, body string
		success              bool
	}{
		{"responses complete", "responses", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n", true},
		{"completed with incomplete status", "responses", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"incomplete\"}}\n\n", false},
		{"done with failed status", "responses", "data: {\"type\":\"response.done\",\"response\":{\"status\":\"failed\"}}\n\n", false},
		{"completed with error", "responses", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"error\":{\"code\":\"failed\"}}}\n\n", false},
		{"incomplete terminal", "responses", "data: {\"type\":\"response.incomplete\"}\n\n", false},
		{"missing terminal", "responses", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\"}\n\n", false},
		{"chat stop", "chat", "data: {\"choices\":[{\"delta\":{\"content\":\"21\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", true},
		{"chat length", "chat", "data: {\"choices\":[{\"delta\":{\"content\":\"21\"},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n", false},
		{"chat no finish", "chat", "data: {\"choices\":[{\"delta\":{\"content\":\"21\"}}]}\n\ndata: [DONE]\n\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			ctx := withPelicanTestOptions(context.Background(), pelicanTestOptions{strictComplete: true})
			c.Request = httptest.NewRequest("POST", "/", nil).WithContext(ctx)
			service := &AccountTestService{}
			var err error
			if tc.protocol == "chat" {
				err = service.processOpenAIChatCompletionsStream(c, strings.NewReader(tc.body))
			} else {
				err = service.processOpenAIStream(c, strings.NewReader(tc.body))
			}
			if tc.success {
				require.NoError(t, err)
				require.Contains(t, recorder.Body.String(), `"success":true`)
			} else {
				require.Error(t, err)
				require.NotContains(t, recorder.Body.String(), `"success":true`)
			}
		})
	}
}

type qualityMonitorTestAccounts struct {
	AccountRepository
	items map[int64]*Account
}

func (a *qualityMonitorTestAccounts) GetByID(_ context.Context, id int64) (*Account, error) {
	return a.items[id], nil
}
func (a *qualityMonitorTestAccounts) GetByIDs(_ context.Context, ids []int64) ([]*Account, error) {
	var result []*Account
	for _, id := range ids {
		if a.items[id] != nil {
			result = append(result, a.items[id])
		}
	}
	return result, nil
}

type qualityMonitorTestRepo struct {
	AccountQualityMonitorRepository
	mu        sync.Mutex
	records   map[int64]*AccountQualityMonitorRecord
	finished  int
	released  int
	loseLease bool
	postponed int
}

func copyQualityMonitorRecord(record *AccountQualityMonitorRecord) *AccountQualityMonitorRecord {
	if record == nil {
		return nil
	}
	raw, _ := json.Marshal(record)
	var copy AccountQualityMonitorRecord
	_ = json.Unmarshal(raw, &copy)
	return &copy
}
func (r *qualityMonitorTestRepo) Get(_ context.Context, id int64, _ bool) (*AccountQualityMonitorRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return copyQualityMonitorRecord(r.records[id]), nil
}
func (r *qualityMonitorTestRepo) Batch(_ context.Context, ids []int64) (map[int64]*AccountQualityMonitorRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make(map[int64]*AccountQualityMonitorRecord, len(ids))
	for _, id := range ids {
		if record := r.records[id]; record != nil {
			result[id] = copyQualityMonitorRecord(record)
		}
	}
	return result, nil
}
func (r *qualityMonitorTestRepo) Save(_ context.Context, id int64, config AccountQualityMonitorConfig, now time.Time) (*AccountQualityMonitorRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.save(id, config, now), nil
}
func (r *qualityMonitorTestRepo) Ensure(_ context.Context, id int64, config AccountQualityMonitorConfig, now time.Time) (*AccountQualityMonitorRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if stored := r.records[id]; stored != nil && stored.Scope == config.Scope {
		return copyQualityMonitorRecord(stored), nil
	}
	return r.save(id, config, now), nil
}
func (r *qualityMonitorTestRepo) save(id int64, config AccountQualityMonitorConfig, now time.Time) *AccountQualityMonitorRecord {
	stored := r.records[id]
	if stored == nil {
		stored = &AccountQualityMonitorRecord{AccountID: id}
		r.records[id] = stored
	}
	if stored.Scope != config.Scope || stored.Model != config.Model {
		stored.ResultVersion++
		stored.LatestAttempt, stored.LastValid = nil, nil
	}
	stored.AccountQualityMonitorConfig = config
	stored.NextDueAt = nil
	if config.Enabled {
		stored.NextDueAt = &now
	}
	return copyQualityMonitorRecord(stored)
}
func (r *qualityMonitorTestRepo) ListDue(_ context.Context, now time.Time, _ int) ([]*AccountQualityMonitorRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result []*AccountQualityMonitorRecord
	for _, stored := range r.records {
		if stored.Enabled && stored.NextDueAt != nil && !stored.NextDueAt.After(now) {
			result = append(result, copyQualityMonitorRecord(stored))
		}
	}
	return result, nil
}
func (r *qualityMonitorTestRepo) Postpone(_ context.Context, record *AccountQualityMonitorRecord, next time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	stored := r.records[record.AccountID]
	if stored.Scope == record.Scope && stored.ResultVersion == record.ResultVersion {
		stored.NextDueAt = &next
		r.postponed++
	}
	return nil
}
func (r *qualityMonitorTestRepo) Claim(ctx context.Context, record *AccountQualityMonitorRecord, run string, _, until time.Time, automatic bool) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	stored := r.records[record.AccountID]
	if stored.RunID != "" || automatic && !stored.Enabled {
		return false, nil
	}
	stored.RunID = run
	stored.LeaseUntil = &until
	return true, nil
}
func (r *qualityMonitorTestRepo) Renew(_ context.Context, id int64, run string, until time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.loseLease {
		return false, nil
	}
	stored := r.records[id]
	if stored.RunID != run {
		return false, nil
	}
	stored.LeaseUntil = &until
	return true, nil
}
func (r *qualityMonitorTestRepo) Finish(_ context.Context, record *AccountQualityMonitorRecord, run string, result *AccountQualityMonitorResult, _ time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	stored := r.records[record.AccountID]
	if stored.RunID != run || stored.ResultVersion != record.ResultVersion {
		return false, nil
	}
	stored.LatestAttempt = result
	if result.State == "passed" || result.State == "failed" {
		stored.LastValid = result
	}
	r.finished++
	return true, nil
}
func (r *qualityMonitorTestRepo) Release(ctx context.Context, id int64, run string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if r.records[id].RunID == run {
		r.records[id].RunID = ""
		r.records[id].LeaseUntil = nil
	}
	r.released++
	return nil
}

func newQualityMonitorTestService() *AccountQualityMonitorService {
	accounts := &qualityMonitorTestAccounts{items: make(map[int64]*Account)}
	repo := &qualityMonitorTestRepo{records: make(map[int64]*AccountQualityMonitorRecord)}
	for id := int64(1); id <= 3; id++ {
		a := &Account{ID: id, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Credentials: map[string]any{"api_key": "fixture"}}
		accounts.items[id] = a
		repo.records[id] = &AccountQualityMonitorRecord{AccountID: id, AccountQualityMonitorConfig: defaultAccountQualityConfig(a), ResultVersion: 1}
	}
	return NewAccountQualityMonitorService(repo, accounts, nil)
}

func awaitQualityMonitorRelease(t *testing.T, s *AccountQualityMonitorService, count int) {
	t.Helper()
	repo := s.repo.(*qualityMonitorTestRepo)
	require.Eventually(t, func() bool { repo.mu.Lock(); defer repo.mu.Unlock(); return repo.released >= count }, time.Second, time.Millisecond)
}

func TestAccountQualityManualDisabledDeduplicatedAndBounded(t *testing.T) {
	s := newQualityMonitorTestService()
	defer s.Stop()
	started := make(chan int64, 3)
	s.execute = func(ctx context.Context, id int64, _ string) (*ScheduledTestResult, error) {
		started <- id
		<-ctx.Done()
		return nil, ctx.Err()
	}
	for _, id := range []int64{1, 1, 2} {
		view, err := s.Start(context.Background(), id, AccountActivityScope(s.accounts.(*qualityMonitorTestAccounts).items[id]))
		require.NoError(t, err)
		require.False(t, view.Enabled)
		require.Equal(t, "running", view.State)
	}
	<-started
	<-started
	select {
	case <-started:
		t.Fatal("duplicate request started another task")
	default:
	}
	_, err := s.Start(context.Background(), 3, AccountActivityScope(s.accounts.(*qualityMonitorTestAccounts).items[3]))
	require.ErrorIs(t, err, ErrAccountQualityBusy)
	s.Stop()
	awaitQualityMonitorRelease(t, s, 2)
	_, err = s.Start(context.Background(), 1, AccountActivityScope(s.accounts.(*qualityMonitorTestAccounts).items[1]))
	require.ErrorIs(t, err, ErrAccountQualityStopped)
	repo := s.repo.(*qualityMonitorTestRepo)
	repo.mu.Lock()
	require.Zero(t, repo.finished)
	repo.mu.Unlock()
}

func TestAccountQualityTaskOutlivesRequestAndJudgesBeforeTruncation(t *testing.T) {
	s := newQualityMonitorTestService()
	defer s.Stop()
	started := make(chan struct{})
	finish := make(chan struct{})
	cancelled := make(chan struct{}, 1)
	s.execute = func(ctx context.Context, _ int64, _ string) (*ScheduledTestResult, error) {
		close(started)
		select {
		case <-ctx.Done():
			cancelled <- struct{}{}
			return nil, ctx.Err()
		case <-finish:
			return &ScheduledTestResult{Status: "success", ResponseText: strings.Repeat("x", 13000) + "\n21"}, nil
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, err := s.Start(ctx, 1, AccountActivityScope(s.accounts.(*qualityMonitorTestAccounts).items[1]))
	require.NoError(t, err)
	<-started
	cancel()
	close(finish)
	awaitQualityMonitorRelease(t, s, 1)
	select {
	case <-cancelled:
		t.Fatal("request cancellation cancelled a detached monitor task")
	default:
	}
	view, err := s.Get(context.Background(), 1, true)
	require.NoError(t, err)
	require.Equal(t, "passed", view.LatestAttempt.State)
	require.Len(t, []rune(view.LatestAttempt.Text), 12000)
	summary, err := s.Get(context.Background(), 1, false)
	require.NoError(t, err)
	require.Empty(t, summary.LatestAttempt.Text)
}

func TestAccountQualityLeaseLossCancelsAndDiscardsResult(t *testing.T) {
	s := newQualityMonitorTestService()
	defer s.Stop()
	s.renewInterval = time.Millisecond
	s.repo.(*qualityMonitorTestRepo).loseLease = true
	s.execute = func(ctx context.Context, _ int64, _ string) (*ScheduledTestResult, error) {
		<-ctx.Done()
		return &ScheduledTestResult{Status: "success", ResponseText: "21"}, nil
	}
	_, err := s.Start(context.Background(), 1, AccountActivityScope(s.accounts.(*qualityMonitorTestAccounts).items[1]))
	require.NoError(t, err)
	awaitQualityMonitorRelease(t, s, 1)
	view, err := s.Get(context.Background(), 1, true)
	require.NoError(t, err)
	require.Nil(t, view.LatestAttempt)
}

func TestAccountQualityErrorPreservesLastValid(t *testing.T) {
	s := newQualityMonitorTestService()
	defer s.Stop()
	repo := s.repo.(*qualityMonitorTestRepo)
	repo.records[1].LastValid = &AccountQualityMonitorResult{State: "passed", Answer: "21"}
	s.execute = func(context.Context, int64, string) (*ScheduledTestResult, error) {
		return nil, errors.New("private upstream error")
	}
	_, err := s.Start(context.Background(), 1, AccountActivityScope(s.accounts.(*qualityMonitorTestAccounts).items[1]))
	require.NoError(t, err)
	awaitQualityMonitorRelease(t, s, 1)
	view, err := s.Get(context.Background(), 1, true)
	require.NoError(t, err)
	require.Equal(t, "error", view.LatestAttempt.State)
	require.Equal(t, "passed", view.LastValid.State)
	require.NotContains(t, view.LatestAttempt.Error, "private")
}

func TestAccountQualityRunDuePausesWithoutDisablingAndResetsIdentity(t *testing.T) {
	for _, changedIdentity := range []bool{false, true} {
		name := "quota pause"
		if changedIdentity {
			name = "identity replacement"
		}
		t.Run(name, func(t *testing.T) {
			s := newQualityMonitorTestService()
			defer s.Stop()
			now := time.Now()
			s.now = func() time.Time { return now }
			repo := s.repo.(*qualityMonitorTestRepo)
			record := repo.records[1]
			record.Enabled, record.NextDueAt = true, &now
			record.LastValid = &AccountQualityMonitorResult{State: "passed", Answer: "21"}
			account := s.accounts.(*qualityMonitorTestAccounts).items[1]
			if changedIdentity {
				account.Credentials["api_key"] = "replacement"
			} else {
				reset := now.Add(time.Hour)
				account.RateLimitResetAt = &reset
			}
			s.execute = func(context.Context, int64, string) (*ScheduledTestResult, error) {
				t.Error("paused or replaced identity must not be dispatched")
				return nil, nil
			}
			s.RunDue(context.Background(), now)
			view, err := s.Get(context.Background(), 1, true)
			require.NoError(t, err)
			if changedIdentity {
				require.False(t, view.Enabled)
				require.Nil(t, view.LastValid)
				require.Equal(t, AccountActivityScope(account), view.Scope)
				require.Equal(t, "gpt-6.1-sol", view.Model)
				require.Equal(t, "untested", view.State)
			} else {
				require.True(t, view.Enabled)
				require.Equal(t, "paused", view.State)
				require.Equal(t, "passed", view.LastValid.State)
				require.Equal(t, 1, repo.postponed)
				require.Equal(t, now.Add(time.Minute), *repo.records[1].NextDueAt)
				require.Nil(t, view.NextDueAt)
			}
		})
	}
}

func TestAccountQualityRejectsMappedMediaModels(t *testing.T) {
	s := newQualityMonitorTestService()
	defer s.Stop()
	account := s.accounts.(*qualityMonitorTestAccounts).items[1]
	account.Credentials["model_mapping"] = map[string]any{"image-alias": "gpt-image-2", "text-alias": "gpt-6.1-sol"}
	for _, model := range []string{"image-alias", "gpt-image-2", "dall-e-3", "gpt-4o-mini-tts", "gpt-realtime", "text-embedding-3-small", "omni-moderation-latest", "sora-2"} {
		t.Run(model, func(t *testing.T) {
			config := defaultAccountQualityConfig(account)
			config.Model = model
			_, err := s.Save(context.Background(), 1, config)
			require.ErrorIs(t, err, ErrAccountQualityTextModel)
		})
	}
	require.True(t, IsAccountQualityTextModel(account, "text-alias"))
	require.True(t, IsAccountQualityTextModel(account, "provider/custom-reasoner"))
	require.True(t, IsAccountQualityTextModel(account, "vendor-image-aware-chat"))
	require.True(t, IsAccountQualityTextModel(account, "acme-embedding-explainer"))
	s.repo.(*qualityMonitorTestRepo).records[1].Model = "image-alias"
	_, err := s.Start(context.Background(), 1, AccountActivityScope(account))
	require.ErrorIs(t, err, ErrAccountQualityTextModel)
}

func TestAccountQualityShadowScopeTracksParentIdentity(t *testing.T) {
	s := newQualityMonitorTestService()
	defer s.Stop()
	accounts := s.accounts.(*qualityMonitorTestAccounts)
	parentID := int64(10)
	parent := &Account{ID: parentID, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
		Credentials: map[string]any{"chatgpt_account_id": "parent-identity", "access_token": "old"}}
	child := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, ParentAccountID: &parentID, GroupIDs: []int64{1}}
	accounts.items[1], accounts.items[parentID] = child, parent
	scope := AccountActivityScopeWithCredentialAccount(child, parent)
	require.NotEmpty(t, scope)
	require.Empty(t, AccountActivityScope(child), "an unresolved shadow must fail closed")
	parent.Credentials["access_token"] = "refreshed"
	require.Equal(t, scope, AccountActivityScopeWithCredentialAccount(child, parent))
	record := s.repo.(*qualityMonitorTestRepo).records[1]
	record.Scope = scope
	record.LastValid = &AccountQualityMonitorResult{State: "passed", Answer: "21"}
	views, err := s.BatchForAccounts(WithObserverScope(context.Background(), []int64{1}), []*Account{child})
	require.NoError(t, err)
	require.Equal(t, scope, views[1].Scope)
	require.Equal(t, "passed", views[1].LastValid.State)
	parent.Credentials["chatgpt_account_id"] = "new-parent-identity"
	view, err := s.Get(context.Background(), 1, true)
	require.NoError(t, err)
	require.NotEqual(t, scope, view.Scope)
	require.Nil(t, view.LastValid)
}

type qualityMonitorHTTPUpstream struct {
	HTTPUpstream
	request   *http.Request
	accountID int64
	body      []byte
}

func (u *qualityMonitorHTTPUpstream) DoWithTLS(req *http.Request, _ string, accountID int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	u.request, u.accountID = req, accountID
	u.body, _ = io.ReadAll(req.Body)
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"content\":\"21\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))}, nil
}

func TestAccountQualityExecutionUsesExactPromptAndSelectedAccount(t *testing.T) {
	accounts := &qualityMonitorTestAccounts{items: map[int64]*Account{
		7: {ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive,
			Extra:       map[string]any{"openai_responses_supported": false},
			Credentials: map[string]any{"api_key": "selected-fixture", "base_url": "https://provider.example"}},
		8: {ID: 8, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive,
			Credentials: map[string]any{"api_key": "other-fixture"}},
	}}
	upstream := &qualityMonitorHTTPUpstream{}
	tests := &AccountTestService{accountRepo: accounts, httpUpstream: upstream, cfg: &config.Config{}}
	s := NewAccountQualityMonitorService(nil, accounts, tests)
	defer s.Stop()
	result, err := s.execute(context.Background(), 7, "gpt-6.1-sol")
	require.NoError(t, err)
	require.Equal(t, "success", result.Status, result.ErrorMessage)
	require.Equal(t, "21", result.ResponseText)
	require.Equal(t, int64(7), upstream.accountID)
	require.Equal(t, "Bearer selected-fixture", upstream.request.Header.Get("Authorization"))
	require.Equal(t, "/v1/chat/completions", upstream.request.URL.Path)
	var payload struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(upstream.body, &payload))
	require.Equal(t, "gpt-6.1-sol", payload.Model)
	var prompts []string
	for _, message := range payload.Messages {
		if message.Role == "user" {
			prompts = append(prompts, message.Content)
		}
	}
	require.Equal(t, []string{CandyPrompt + "\n最终结果只输出整数"}, prompts)
	require.NotContains(t, string(upstream.body), "只输出最终答案，不要解释。")
	state, answer := judgeAccountQualityAnswer(result.ResponseText)
	require.Equal(t, "passed", state)
	require.Equal(t, "21", answer)
}

func TestAccountQualityPanicReleasesOwnershipAndCapacity(t *testing.T) {
	s := newQualityMonitorTestService()
	defer s.Stop()
	s.execute = func(context.Context, int64, string) (*ScheduledTestResult, error) { panic("provider adapter panic") }
	_, err := s.Start(context.Background(), 1, AccountActivityScope(s.accounts.(*qualityMonitorTestAccounts).items[1]))
	require.NoError(t, err)
	awaitQualityMonitorRelease(t, s, 1)
	require.Eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.active) == 0 && len(s.slots) == 0 }, time.Second, time.Millisecond)
}
