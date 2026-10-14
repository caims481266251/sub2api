package service

import (
	"context"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var ErrHostingPoolNotFound = infraerrors.NotFound("HOSTING_POOL_NOT_FOUND", "托管池不存在")
var ErrHostingPoolInUse = infraerrors.Conflict("HOSTING_POOL_IN_USE", "请先移出池内账号，并修改使用此池的 Key，再删除托管池")
var ErrHostingPoolNameExists = infraerrors.Conflict("HOSTING_POOL_NAME_EXISTS", "已存在同名托管池")

type HostingPool struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	AccountCount int    `json:"account_count"`
	KeyCount     int    `json:"key_count"`
}

func normalizeHostingPoolID(id *int64) *int64 {
	if id == nil || *id == 0 {
		return nil
	}
	return id
}

func sameHostingPool(left, right *int64) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func (s *HostingService) ResolvePool(ctx context.Context, userID int64, poolID *int64) error {
	_, err := s.resolvePool(ctx, userID, poolID)
	return err
}

func (s *HostingService) resolvePool(ctx context.Context, userID int64, poolID *int64) (*HostingPool, error) {
	if poolID == nil || *poolID == 0 {
		return &HostingPool{}, nil
	}
	if *poolID < 0 {
		return nil, ErrHostingPoolNotFound
	}
	return s.repo.GetOwnedPool(ctx, userID, *poolID)
}

func (s *HostingService) Pools(ctx context.Context, userID int64) ([]HostingPool, error) {
	return s.repo.ListPools(ctx, userID)
}

func (s *HostingService) SavePool(ctx context.Context, userID, id int64, name string) (*HostingPool, error) {
	if _, err := s.requireEnabled(ctx, userID); err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 100 || id < 0 {
		return nil, infraerrors.BadRequest("HOSTING_POOL_NAME_INVALID", "请输入 1 到 100 个字符的托管池名称")
	}
	return s.repo.SavePool(ctx, userID, id, name)
}

func (s *HostingService) DeletePool(ctx context.Context, userID, id int64) error {
	if id <= 0 {
		return ErrHostingPoolNotFound
	}
	return s.repo.DeletePool(ctx, userID, id)
}
