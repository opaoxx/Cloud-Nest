package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UserProfileService 读取用户资料并复查账号状态，供登录结果与当前用户接口使用。
type UserProfileService struct {
	// findProfile 绑定 Repository.FindUserProfile，查询负责根目录与配额数据检查。
	findProfile func(context.Context, uint64) (*domain.UserProfile, error)
}

// NewUserProfileService 保存资料查询能力，缺少依赖属于装配错误。
func NewUserProfileService(findProfile func(context.Context, uint64) (*domain.UserProfile, error)) *UserProfileService {
	// 1. 确认查询方法已经提供，保存供每次请求调用。
	if findProfile == nil {
		panic("用户资料服务需要资料查询方法")
	}
	return &UserProfileService{findProfile: findProfile}
}

// Get 读取已验证用户的资料，成功返回 ACTIVE 账号的资料副本。
// userID 来自凭证验证或 Actor，根目录及配额检查由查询方法完成。
func (profileService *UserProfileService) Get(ctx context.Context, userID uint64) (*domain.UserProfile, error) {
	// 1. 检查用户 ID 和请求上下文。
	if ctx == nil || userID == 0 {
		return nil, fmt.Errorf("读取用户资料需要上下文和非零用户 ID")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 2. 查询资料，处理取消、未找到及查询故障。
	profile, queryError := profileService.findProfile(ctx, userID)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if errors.Is(queryError, domain.ErrAccountNotFound) {
		return nil, domain.ErrAccountNotFound
	}
	if queryError != nil {
		return nil, fmt.Errorf("读取用户资料失败: %w", queryError)
	}

	// 3. 检查资料归属与账号状态。
	if profile == nil {
		return nil, domain.ErrInvalidAccountData
	}
	result := *profile
	if result.ID != userID || result.RootEntryID == 0 {
		return nil, domain.ErrInvalidAccountData
	}
	switch result.Status {
	case "ACTIVE":
	case "DISABLED":
		return nil, domain.ErrAccountDisabled
	default:
		return nil, domain.ErrInvalidAccountData
	}

	// 4. 返回用户资料副本。
	return &result, nil
}
