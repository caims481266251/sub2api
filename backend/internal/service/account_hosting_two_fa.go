package service

import (
	"context"
	"encoding/json"
	"net/url"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// Reuse the admin login engine, with separate ownership and no enrollment in
// platform credential operations. Tokens never leave the hosting service.
type hostingTwoFAProvider interface {
	GetConfig(context.Context) (AccountTokenGuardConfig, error)
	StartTwoFALoginForOperations(context.Context, AccountTokenGuardReloginAccount) (*OpenAITwoFALoginJob, error)
	TwoFALogin(string) (*OpenAITwoFALoginJob, bool)
	DeleteTwoFALogin(string)
}

type hostingTwoFASession struct {
	mu                sync.Mutex
	userID, replaceID int64
	poolID            *int64
	providerID        string
	expiry            *time.Timer
	deleted           bool
}

type HostingTwoFAJob struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

var errHostingTwoFAJob = infraerrors.NotFound("HOSTING_LOGIN_NOT_FOUND", "登录任务不存在或已过期")

func (s *HostingService) SetTwoFALoginService(provider hostingTwoFAProvider) { s.twoFA = provider }

func (s *HostingService) TwoFAAvailability(ctx context.Context) (bool, string) {
	if s.twoFA == nil {
		return false, ""
	}
	cfg, err := s.twoFA.GetConfig(ctx)
	if err != nil || validateGuardHTTPURL(cfg.ReloginEndpoint, "relogin_endpoint") != nil {
		return false, ""
	}
	u, err := url.Parse(cfg.ReloginEndpoint)
	if err != nil {
		return false, ""
	}
	return true, u.Host
}

func (s *HostingService) StartTwoFALogin(ctx context.Context, userID, replaceID int64, poolID *int64, entry AccountTokenGuardReloginAccount) (*HostingTwoFAJob, error) {
	cfg, err := s.requireEnabled(ctx, userID)
	if err != nil {
		return nil, err
	}
	if err = ValidateOpenAITwoFALogin(entry); err != nil {
		return nil, infraerrors.BadRequest("HOSTING_LOGIN_INVALID", "请填写有效的邮箱、密码和 2FA 密钥")
	}
	if replaceID != 0 {
		if err = s.checkOwned(ctx, userID, replaceID); err != nil {
			return nil, err
		}
		poolID = nil
	} else {
		accounts, listErr := s.repo.ListOwned(ctx, userID)
		if listErr != nil {
			return nil, listErr
		}
		if len(accounts) >= cfg.DefaultSeats {
			return nil, ErrHostingSeatsFull
		}
	}
	poolID = normalizeHostingPoolID(poolID)
	if err = s.ResolvePool(ctx, userID, poolID); err != nil {
		return nil, err
	}
	if available, _ := s.TwoFAAvailability(ctx); !available {
		return nil, infraerrors.New(503, "HOSTING_LOGIN_UNAVAILABLE", "2FA 登录服务暂未配置，请使用授权登录或 auth.json 导入")
	}
	s.twoFAMu.Lock()
	defer s.twoFAMu.Unlock()
	for _, session := range s.twoFAJobs {
		if session.userID == userID {
			return nil, infraerrors.Conflict("HOSTING_LOGIN_RUNNING", "已有登录任务，请完成或取消后再试")
		}
	}
	job, err := s.twoFA.StartTwoFALoginForOperations(ctx, entry)
	if err != nil {
		return nil, infraerrors.New(503, "HOSTING_LOGIN_UNAVAILABLE", "无法启动 2FA 登录，请稍后重试")
	}
	id := guardRandomID()
	if s.twoFAJobs == nil {
		s.twoFAJobs = make(map[string]*hostingTwoFASession)
	}
	session := &hostingTwoFASession{userID: userID, replaceID: replaceID, poolID: poolID, providerID: job.ID}
	s.twoFAJobs[id] = session
	session.expiry = time.AfterFunc(30*time.Minute, func() { _ = s.DeleteTwoFALogin(userID, id) })
	return &HostingTwoFAJob{ID: id, Status: job.Status}, nil
}

func (s *HostingService) ownedTwoFASession(userID int64, id string) (*hostingTwoFASession, error) {
	s.twoFAMu.Lock()
	defer s.twoFAMu.Unlock()
	session := s.twoFAJobs[id]
	if session == nil || session.userID != userID {
		return nil, errHostingTwoFAJob
	}
	return session, nil
}

func (s *HostingService) TwoFALogin(ctx context.Context, userID int64, id string) (*HostingTwoFAJob, error) {
	if _, err := s.requireEnabled(ctx, userID); err != nil {
		return nil, err
	}
	session, err := s.ownedTwoFASession(userID, id)
	if err != nil {
		return nil, err
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.deleted {
		return nil, errHostingTwoFAJob
	}
	job, ok := s.twoFA.TwoFALogin(session.providerID)
	if !ok {
		s.deleteTwoFASession(id, session)
		return nil, errHostingTwoFAJob
	}
	return &HostingTwoFAJob{ID: id, Status: job.Status}, nil
}

func (s *HostingService) DeleteTwoFALogin(userID int64, id string) error {
	session, err := s.ownedTwoFASession(userID, id)
	if err != nil {
		return err
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	s.deleteTwoFASession(id, session)
	return nil
}

func (s *HostingService) deleteTwoFASession(id string, session *hostingTwoFASession) {
	session.deleted = true
	session.expiry.Stop()
	s.twoFA.DeleteTwoFALogin(session.providerID)
	s.twoFAMu.Lock()
	delete(s.twoFAJobs, id)
	s.twoFAMu.Unlock()
}

func (s *HostingService) CompleteTwoFALogin(ctx context.Context, userID int64, id string, input *HostingAccountSettings) (*Account, error) {
	if _, err := s.requireEnabled(ctx, userID); err != nil {
		return nil, err
	}
	if err := s.prepareAccountCreationSettings(ctx, userID, input); err != nil {
		return nil, err
	}
	session, err := s.ownedTwoFASession(userID, id)
	if err != nil {
		return nil, err
	}
	if !session.mu.TryLock() {
		return nil, infraerrors.Conflict("HOSTING_LOGIN_BUSY", "登录任务正在保存，请稍后重试")
	}
	defer session.mu.Unlock()
	if session.deleted {
		return nil, errHostingTwoFAJob
	}
	if session.replaceID == 0 && input != nil && input.HostingPoolID != nil && !sameHostingPool(session.poolID, normalizeHostingPoolID(input.HostingPoolID)) {
		return nil, infraerrors.BadRequest("HOSTING_LOGIN_POOL_CHANGED", "所属池已改变，请返回配置后重新登录")
	}
	job, ok := s.twoFA.TwoFALogin(session.providerID)
	if !ok {
		s.deleteTwoFASession(id, session)
		return nil, errHostingTwoFAJob
	}
	if job.Status != "succeeded" {
		return nil, infraerrors.Conflict("HOSTING_LOGIN_NOT_READY", "2FA 登录尚未成功")
	}
	content, err := json.Marshal(job.Credential)
	if err != nil {
		return nil, infraerrors.BadRequest("HOSTING_LOGIN_INVALID", "登录凭证无效，请重新登录")
	}
	// Refresh verifies identity and subscription, then persists credentials and
	// selected preferences in the same seat-checked account transaction.
	a, err := s.ImportWithSettings(ctx, userID, session.replaceID, "Codex", string(content), input, session.poolID)
	if err != nil {
		return nil, err
	} // Keep the verified job for a save retry.
	s.deleteTwoFASession(id, session)
	return a, nil
}
