package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const HostingSettingKey = "account_hosting"
const RouteModeHosted = "hosted"

var ErrHostingDisabled = infraerrors.Forbidden("HOSTING_DISABLED", "账号托管尚未开放")
var ErrHostingSeatsFull = infraerrors.Conflict("HOSTING_SEATS_FULL", "托管席位已用完")
var ErrHostingIdentityConflict = infraerrors.Conflict("HOSTING_ACCOUNT_EXISTS", "该账号已被托管，不能重复添加")
var ErrHostingAccountNotFound = infraerrors.NotFound("HOSTING_ACCOUNT_NOT_FOUND", "托管账号不存在")
var ErrHostingFallbackInvalid = infraerrors.BadRequest("HOSTING_FALLBACK_INVALID", "请选择有权限的 OpenAI 余额分组，或关闭兜底")

type HostingSettings struct {
	Enabled        bool    `json:"enabled"`
	DefaultSeats   int     `json:"default_seats"`
	RateMultiplier float64 `json:"rate_multiplier"`
}

type HostingUserPolicy struct {
	SeatLimit      *int     `json:"seat_limit"`
	RateMultiplier *float64 `json:"rate_multiplier"`
}

type HostingSummary struct {
	StandardCost float64 `json:"standard_cost"`
	HostingCost  float64 `json:"hosting_cost"`
	FallbackCost float64 `json:"fallback_cost"`
}

type HostingRepository interface {
	Billing(context.Context, int64, int64, string, int, int) (HostingBilling, error)
	ListAllHosted(context.Context, string, int, int) ([]HostingAdminAccount, int, error)
	ListPools(context.Context, int64) ([]HostingPool, error)
	GetOwnedPool(context.Context, int64, int64) (*HostingPool, error)
	SavePool(context.Context, int64, int64, string) (*HostingPool, error)
	DeletePool(context.Context, int64, int64) error
	GetUserPolicy(context.Context, int64) (HostingUserPolicy, error)
	SetUserPolicy(context.Context, int64, HostingUserPolicy) error
	ListOwned(context.Context, int64) ([]Account, error)
	SaveOwned(context.Context, int64, *Account, int64, int, ...*HostingAccountSettings) (*Account, error)
	ChangeOwned(context.Context, int64, int64, string) error
	UpdateOwnedSettings(context.Context, int64, int64, HostingAccountSettings) (*Account, error)
	Summary(context.Context, int64) (HostingSummary, error)
}

type HostingAdminAccount struct {
	Account   Account
	UserID    int64
	UserEmail string
	Username  string
	PoolName  string
}

func (s *HostingService) AdminAccounts(ctx context.Context, search string, page, pageSize int) ([]HostingAdminAccount, int, error) {
	return s.repo.ListAllHosted(ctx, strings.TrimSpace(search), page, pageSize)
}

type hostingOAuthSession struct {
	userID, replaceID int64
	expiresAt         time.Time
	poolID            *int64
}

type HostingService struct {
	repo       HostingRepository
	settings   SettingRepository
	oauth      *OpenAIOAuthService
	groups     GroupRepository
	users      UserRepository
	cfg        *config.Config
	sessions   sync.Map
	sessionsMu sync.Mutex
	twoFA      hostingTwoFAProvider
	twoFAMu    sync.Mutex
	twoFAJobs  map[string]*hostingTwoFASession
}

func NewHostingService(repo HostingRepository, settings SettingRepository, oauth *OpenAIOAuthService, groups GroupRepository, users UserRepository, cfg *config.Config) *HostingService {
	return &HostingService{repo: repo, settings: settings, oauth: oauth, groups: groups, users: users, cfg: cfg}
}

func (s *HostingService) Settings(ctx context.Context) (HostingSettings, error) {
	cfg := HostingSettings{DefaultSeats: 1, RateMultiplier: 0.03}
	raw, err := s.settings.GetValue(ctx, HostingSettingKey)
	if err != nil {
		if !errors.Is(err, ErrSettingNotFound) {
			return cfg, err
		}
		return cfg, nil
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return cfg, err
	}
	if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		cfg.Enabled = false
	}
	return cfg, nil
}

func (s *HostingService) UpdateSettings(ctx context.Context, cfg HostingSettings) error {
	if cfg.DefaultSeats < 0 || cfg.DefaultSeats > 1000 || !validHostingRate(cfg.RateMultiplier) {
		return infraerrors.BadRequest("HOSTING_SETTINGS_INVALID", "席位或倍率无效")
	}
	if cfg.Enabled && s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		return ErrHostingDisabled
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return s.settings.Set(ctx, HostingSettingKey, string(raw))
}

func validHostingRate(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 999999.9999
}
func hostingFallbackGroup(g *Group) bool {
	return g != nil && g.IsActive() && g.Platform == PlatformOpenAI && !g.IsSubscriptionType()
}

func (s *HostingService) Policy(ctx context.Context, userID int64) (HostingSettings, HostingUserPolicy, error) {
	cfg, err := s.Settings(ctx)
	if err != nil {
		return cfg, HostingUserPolicy{}, err
	}
	p, err := s.repo.GetUserPolicy(ctx, userID)
	if err != nil {
		return cfg, p, err
	}
	if p.SeatLimit != nil {
		cfg.DefaultSeats = *p.SeatLimit
	}
	if p.RateMultiplier != nil {
		cfg.RateMultiplier = *p.RateMultiplier
	}
	return cfg, p, nil
}

func (s *HostingService) SetUserPolicy(ctx context.Context, userID int64, p HostingUserPolicy) error {
	if p.SeatLimit != nil && (*p.SeatLimit < 0 || *p.SeatLimit > 1000) || p.RateMultiplier != nil && !validHostingRate(*p.RateMultiplier) {
		return infraerrors.BadRequest("HOSTING_POLICY_INVALID", "席位或倍率无效")
	}
	if _, err := s.users.GetByID(ctx, userID); err != nil {
		return err
	}
	return s.repo.SetUserPolicy(ctx, userID, p)
}

func (s *HostingService) Accounts(ctx context.Context, userID int64) ([]Account, error) {
	return s.repo.ListOwned(ctx, userID)
}

// HostedAccountUsage uses the same Codex window normalization as the account
// management usage endpoint without probing upstream for every listed account.
func HostedAccountUsage(a *Account) *UsageInfo {
	usage := &UsageInfo{}
	if a != nil {
		applyExtraToUsage(usage, a.Extra, time.Now())
	}
	return usage
}

// The stored quota snapshot stops blocking after its upstream reset time.
// Use the same window parser as scheduling so the UI reflects recovery.
func HostedAccountAvailability(a *Account, now time.Time) (string, *time.Time) {
	if a.Status == StatusDisabled || a.Status == "inactive" {
		return "paused", nil
	}
	if a.Status == StatusError {
		message := strings.ToLower(a.ErrorMessage)
		for _, hint := range []string{"401", "unauthorized", "invalid_grant", "refresh", "token", "login", "auth", "登录", "凭证"} {
			if strings.Contains(message, hint) {
				return "login_required", nil
			}
		}
		return "temporarily_unavailable", nil
	}
	reset := a.RateLimitResetAt
	for _, window := range []string{"5h", "7d"} {
		if used, ok := resolveOpenAIQuotaUtilization(a.Extra, window, now); ok && used >= 1 {
			if at, ok := openAICodexWindowResetAt(a.Extra, window); ok && now.Before(at) && (reset == nil || at.After(*reset)) {
				reset = &at
			}
		}
	}
	if reset != nil && now.Before(*reset) {
		return "quota_exhausted", reset
	}
	if !a.IsSchedulable() {
		until := a.TempUnschedulableUntil
		if until == nil || a.OverloadUntil != nil && a.OverloadUntil.After(*until) {
			until = a.OverloadUntil
		}
		return "temporarily_unavailable", until
	}
	return "active", nil
}
func (s *HostingService) Summary(ctx context.Context, userID int64) (HostingSummary, error) {
	return s.repo.Summary(ctx, userID)
}

func (s *HostingService) ChangeAccount(ctx context.Context, userID, accountID int64, action string) error {
	if action != "pause" && action != "resume" && action != "delete" && action != "schedule-enable" && action != "schedule-disable" {
		return infraerrors.BadRequest("HOSTING_ACTION_INVALID", "不支持的操作")
	}
	return s.repo.ChangeOwned(ctx, userID, accountID, action)
}

func (s *HostingService) requireEnabled(ctx context.Context, userID int64) (HostingSettings, error) {
	cfg, _, err := s.Policy(ctx, userID)
	if err != nil {
		return cfg, err
	}
	if !cfg.Enabled {
		return cfg, ErrHostingDisabled
	}
	return cfg, nil
}

func (s *HostingService) StartOAuth(ctx context.Context, userID, replaceID int64, poolIDs ...*int64) (*OpenAIAuthURLResult, error) {
	cfg, err := s.requireEnabled(ctx, userID)
	if err != nil {
		return nil, err
	}
	if replaceID != 0 {
		if err := s.checkOwned(ctx, userID, replaceID); err != nil {
			return nil, err
		}
	} else {
		accounts, err := s.repo.ListOwned(ctx, userID)
		if err != nil {
			return nil, err
		}
		if len(accounts) >= cfg.DefaultSeats {
			return nil, ErrHostingSeatsFull
		}
	}
	var poolID *int64
	if replaceID == 0 && len(poolIDs) > 0 {
		poolID = normalizeHostingPoolID(poolIDs[0])
	}
	if err := s.ResolvePool(ctx, userID, poolID); err != nil {
		return nil, err
	}
	// Serialize allocation, not token exchange. Bound both tenant and process
	// memory even when panel rate limiting is disabled or Redis is unavailable.
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	now := time.Now()
	owned, total := 0, 0
	s.sessions.Range(func(k, v any) bool {
		session := v.(hostingOAuthSession)
		if !now.Before(session.expiresAt) {
			s.sessions.Delete(k)
			s.oauth.sessionStore.Delete(k.(string))
		} else {
			total++
			if session.userID == userID {
				owned++
			}
		}
		return true
	})
	if owned >= 3 || total >= 1024 {
		return nil, infraerrors.New(429, "HOSTING_OAUTH_LIMIT", "授权会话过多，请完成已有授权或稍后重试")
	}
	r, err := s.oauth.GenerateAuthURL(ctx, nil, "", PlatformOpenAI, "codex")
	if err != nil {
		return nil, err
	}
	s.sessions.Store(r.SessionID, hostingOAuthSession{userID: userID, replaceID: replaceID, expiresAt: time.Now().Add(10 * time.Minute), poolID: poolID})
	return r, nil
}

func (s *HostingService) CompleteOAuth(ctx context.Context, userID int64, sessionID, code, state, name string, settings ...*HostingAccountSettings) (*Account, error) {
	v, ok := s.sessions.Load(sessionID)
	if !ok {
		return nil, infraerrors.BadRequest("HOSTING_OAUTH_SESSION_INVALID", "授权会话已过期")
	}
	session := v.(hostingOAuthSession)
	if session.userID != userID || time.Now().After(session.expiresAt) {
		return nil, infraerrors.BadRequest("HOSTING_OAUTH_SESSION_INVALID", "授权会话无效")
	}
	var input *HostingAccountSettings
	if len(settings) > 0 {
		input = settings[0]
	}
	if err := s.prepareAccountCreationSettings(ctx, userID, input); err != nil {
		return nil, err
	}
	if session.replaceID == 0 && input != nil && input.HostingPoolID != nil {
		pool := normalizeHostingPoolID(input.HostingPoolID)
		if (pool == nil) != (session.poolID == nil) || pool != nil && *pool != *session.poolID {
			return nil, infraerrors.BadRequest("HOSTING_OAUTH_POOL_CHANGED", "所属池已改变，请返回配置后重新生成授权链接")
		}
	}
	if _, loaded := s.sessions.LoadAndDelete(sessionID); !loaded {
		return nil, infraerrors.BadRequest("HOSTING_OAUTH_SESSION_INVALID", "授权会话已使用")
	}
	token, err := s.oauth.ExchangeCode(ctx, &OpenAIExchangeCodeInput{SessionID: sessionID, Code: code, State: state})
	// A consumed hosting session must not retain its PKCE verifier on failures.
	s.oauth.sessionStore.Delete(sessionID)
	if err != nil {
		return nil, infraerrors.BadRequest("HOSTING_AUTH_INVALID", "授权失败，请重新生成授权链接后登录")
	}
	return s.saveTokenWithSettings(ctx, userID, session.replaceID, name, token, input, session.poolID)
}

func (s *HostingService) Import(ctx context.Context, userID, replaceID int64, name, content string, poolIDs ...*int64) (*Account, error) {
	return s.ImportWithSettings(ctx, userID, replaceID, name, content, nil, poolIDs...)
}

func (s *HostingService) ImportWithSettings(ctx context.Context, userID, replaceID int64, name, content string, input *HostingAccountSettings, poolIDs ...*int64) (*Account, error) {
	if _, err := s.requireEnabled(ctx, userID); err != nil {
		return nil, err
	}
	if replaceID != 0 {
		if err := s.checkOwned(ctx, userID, replaceID); err != nil {
			return nil, err
		}
	}
	var poolID *int64
	if replaceID == 0 && len(poolIDs) > 0 {
		poolID = normalizeHostingPoolID(poolIDs[0])
	}
	if err := s.ResolvePool(ctx, userID, poolID); err != nil {
		return nil, err
	}
	if err := s.prepareAccountCreationSettings(ctx, userID, input); err != nil {
		return nil, err
	}
	var payload struct {
		Tokens struct {
			RefreshToken string `json:"refresh_token"`
		} `json:"tokens"`
		RefreshToken string `json:"refresh_token"`
	}
	if len(content) > 1024*1024 || json.Unmarshal([]byte(content), &payload) != nil {
		return nil, infraerrors.BadRequest("HOSTING_IMPORT_INVALID", "请选择有效的 auth.json 文件")
	}
	rt := strings.TrimSpace(payload.Tokens.RefreshToken)
	if rt == "" {
		rt = strings.TrimSpace(payload.RefreshToken)
	}
	if rt == "" {
		return nil, infraerrors.BadRequest("HOSTING_IMPORT_INVALID", "auth.json 缺少 refresh_token")
	}
	// The upstream refresh verifies the submitted identity instead of trusting editable JSON/JWT claims.
	token, err := s.oauth.RefreshToken(ctx, rt, "")
	if err != nil {
		return nil, infraerrors.BadRequest("HOSTING_AUTH_INVALID", "账号凭证无效，请重新授权")
	}
	return s.saveTokenWithSettings(ctx, userID, replaceID, name, token, input, poolID)
}

func (s *HostingService) checkOwned(ctx context.Context, userID, accountID int64) error {
	accounts, err := s.repo.ListOwned(ctx, userID)
	if err != nil {
		return err
	}
	for _, a := range accounts {
		if a.ID == accountID {
			return nil
		}
	}
	return ErrHostingAccountNotFound
}

func (s *HostingService) saveToken(ctx context.Context, userID, replaceID int64, name string, token *OpenAITokenInfo, poolIDs ...*int64) (*Account, error) {
	return s.saveTokenWithSettings(ctx, userID, replaceID, name, token, nil, poolIDs...)
}

func (s *HostingService) saveTokenWithSettings(ctx context.Context, userID, replaceID int64, name string, token *OpenAITokenInfo, input *HostingAccountSettings, poolIDs ...*int64) (*Account, error) {
	cfg, err := s.requireEnabled(ctx, userID)
	if err != nil {
		return nil, err
	}
	if token == nil || token.ChatGPTAccountID == "" || token.ChatGPTUserID == "" || token.RefreshToken == "" {
		return nil, infraerrors.BadRequest("HOSTING_IDENTITY_MISSING", "无法确认账号身份，请使用 Codex 授权重新登录")
	}
	hash := sha256.Sum256([]byte(token.ChatGPTAccountID + "\x00" + token.ChatGPTUserID))
	identity := hex.EncodeToString(hash[:])
	if err := s.prepareAccountCreationSettings(ctx, userID, input); err != nil {
		return nil, err
	}
	if input != nil && input.Name != nil {
		name = *input.Name
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Codex"
	}
	if len([]rune(name)) > 100 {
		return nil, infraerrors.BadRequest("HOSTING_NAME_INVALID", "账号名称过长")
	}
	a := &Account{Name: name, Platform: PlatformOpenAI, Type: AccountTypeOAuth, OwnerUserID: &userID, HostingIdentity: &identity, Credentials: s.oauth.BuildAccountCredentials(token), Extra: map[string]any{}, Concurrency: 3, Priority: 50, Status: StatusActive, Schedulable: true, AutoPauseOnExpired: true}
	if replaceID == 0 && len(poolIDs) > 0 {
		a.HostingPoolID = normalizeHostingPoolID(poolIDs[0])
		if err := s.ResolvePool(ctx, userID, a.HostingPoolID); err != nil {
			return nil, err
		}
	}
	if !a.IsOpenAIChatGPTSubscription() {
		return nil, infraerrors.BadRequest("HOSTING_SUBSCRIPTION_REQUIRED", "首期仅支持 Codex 订阅账号，请确认订阅套餐后重新授权")
	}
	return s.repo.SaveOwned(ctx, userID, a, replaceID, cfg.DefaultSeats, input)
}

func (s *HostingService) ResolveFallback(ctx context.Context, userID int64, groupID *int64) (*Group, error) {
	if groupID == nil {
		return nil, nil
	}
	g, err := s.groups.GetByID(ctx, *groupID)
	if err != nil || !hostingFallbackGroup(g) {
		return nil, ErrHostingFallbackInvalid
	}
	u, err := s.users.GetByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !u.CanBindGroup(g.ID, g.IsExclusive) {
		return nil, ErrHostingFallbackInvalid
	}
	return g, nil
}

type hostingRequestContextKey struct{}

func HostingAccountAllowed(ctx context.Context, a *Account) bool {
	if a == nil {
		return false
	}
	key, _ := ctx.Value(hostingRequestContextKey{}).(*APIKey)
	if key != nil && key.RouteMode == RouteModeHosted && !key.HostingFallback {
		return a.OwnerUserID != nil && *a.OwnerUserID == key.UserID && sameHostingPool(a.HostingPoolID, key.HostingPoolID)
	}
	return a.OwnerUserID == nil
}

func hostingKeyFromContext(ctx context.Context) *APIKey {
	key, _ := ctx.Value(hostingRequestContextKey{}).(*APIKey)
	return key
}

func (s *HostingService) PrepareRequest(ctx context.Context, key *APIKey) (context.Context, *APIKey, error) {
	if key.RouteMode != RouteModeHosted {
		return ctx, key, nil
	}
	cfg, err := s.requireEnabled(ctx, key.UserID)
	if err != nil {
		return ctx, nil, err
	}
	pool, err := s.resolvePool(ctx, key.UserID, key.HostingPoolID)
	if err != nil {
		return ctx, nil, err
	}
	clone := *key
	clone.HostingPoolName = &pool.Name
	if key.User != nil {
		userClone := *key.User
		clone.User = &userClone
	}
	clone.GroupID = nil
	clone.Group = nil
	clone.HostingFallback = false
	clone.HostingFallbackReason = ""
	clone.HostingRate = &cfg.RateMultiplier
	ctx = context.WithValue(ctx, hostingRequestContextKey{}, &clone)
	accounts, err := s.repo.ListOwned(ctx, key.UserID)
	if err != nil {
		return ctx, nil, err
	}
	available := false
	for i := range accounts {
		if HostingAccountAllowed(ctx, &accounts[i]) && accounts[i].IsSchedulable() {
			available = true
			break
		}
	}
	if !available && clone.HostingFallbackGroupID != nil {
		if err := s.SwitchFallback(ctx); err != nil {
			return ctx, nil, err
		}
	}
	return ctx, &clone, nil
}

func (s *HostingService) SwitchFallback(ctx context.Context) error {
	key := hostingKeyFromContext(ctx)
	if key == nil || key.HostingFallback || key.HostingFallbackGroupID == nil {
		return infraerrors.New(503, "HOSTING_ACCOUNTS_UNAVAILABLE", "托管账号暂时不可用，且未设置兜底分组")
	}
	g, err := s.ResolveFallback(ctx, key.UserID, key.HostingFallbackGroupID)
	if err != nil {
		return err
	}
	key.HostingFallback = true
	key.HostingFallbackReason = "托管池没有可用账号"
	key.Group = g
	key.GroupID = &g.ID
	return nil
}

func (s *APIKeyService) SetHostingService(h *HostingService) { s.hostingService = h }
func (s *APIKeyService) PrepareHostedRequest(ctx context.Context, key *APIKey) (context.Context, *APIKey, error) {
	if key.RouteMode != RouteModeHosted {
		return ctx, key, nil
	}
	if s.hostingService == nil {
		return ctx, nil, ErrHostingDisabled
	}
	return s.hostingService.PrepareRequest(ctx, key)
}
func (s *APIKeyService) SwitchHostedFallback(ctx context.Context) error {
	if s.hostingService == nil {
		return ErrHostingDisabled
	}
	return s.hostingService.SwitchFallback(ctx)
}

// Each new WebSocket turn captures its own rate; an in-progress turn keeps
// the captured value, including when its billing task executes later.
func (s *APIKeyService) RefreshHostedTurnRate(ctx context.Context, key *APIKey) error {
	if key.RouteMode != RouteModeHosted {
		return nil
	}
	if s.hostingService == nil {
		return ErrHostingDisabled
	}
	pool, err := s.hostingService.resolvePool(ctx, key.UserID, key.HostingPoolID)
	if err != nil {
		return err
	}
	key.HostingPoolName = &pool.Name
	cfg, err := s.hostingService.requireEnabled(ctx, key.UserID)
	if err != nil {
		return err
	}
	if key.HostingFallback {
		_, err = s.hostingService.ResolveFallback(ctx, key.UserID, key.HostingFallbackGroupID)
		return err
	}
	key.HostingRate = &cfg.RateMultiplier
	return nil
}

// Revalidation compares persisted routing configuration, then applies the
// current request's chosen path to fresh permissions without reselecting.
func (s *APIKeyService) HostedAuthorizationView(ctx context.Context, latest *APIKey) (*APIKey, error) {
	if latest == nil || latest.RouteMode != RouteModeHosted {
		return latest, nil
	}
	if s.hostingService == nil {
		return nil, ErrHostingDisabled
	}
	if _, err := s.hostingService.requireEnabled(ctx, latest.UserID); err != nil {
		return nil, err
	}
	if err := s.hostingService.ResolvePool(ctx, latest.UserID, latest.HostingPoolID); err != nil {
		return nil, err
	}
	current := hostingKeyFromContext(ctx)
	if current == nil || !current.HostingFallback {
		return latest, nil
	}
	group, err := s.hostingService.ResolveFallback(ctx, latest.UserID, latest.HostingFallbackGroupID)
	if err != nil || group == nil {
		if err == nil {
			err = ErrHostingFallbackInvalid
		}
		return nil, err
	}
	clone := *latest
	clone.Group = group
	clone.GroupID = &group.ID
	if latest.User != nil {
		u := *latest.User
		clone.User = &u
		denied, err := s.GetUserGroupDeniedModels(ctx, latest.UserID)
		if err != nil {
			return nil, err
		}
		clone.User.UserGroupDeniedModels = denied[group.ID]
	}
	return &clone, nil
}

func hostingPrimary(ctx context.Context) bool {
	key := hostingKeyFromContext(ctx)
	return key != nil && !key.HostingFallback
}
func scopeHostingSessionHash(ctx context.Context, hash string) string {
	key := hostingKeyFromContext(ctx)
	if key == nil {
		return hash
	}
	poolID := int64(0)
	if key.HostingPoolID != nil {
		poolID = *key.HostingPoolID
	}
	return fmt.Sprintf("hosted:%d:%d:%d:%s", key.UserID, key.ID, poolID, hash)
}

func (s *OpenAIGatewayService) SetHostingService(h *HostingService) { s.hostingService = h }
