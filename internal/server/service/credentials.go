package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// missingAccountHash 是离线生成、成本为 12 的非账号测试摘要，不是任何账号的凭证。
// 未命中时也执行一次比对，但永远返回凭证错误，不能据此认证身份。
const missingAccountHash = "$2a$12$PLkb00cE2rPYZXJHsERjX.kJQlXmq0bdP1Au0fZhpEARary7R9nIa"

// CredentialService 组织只读账号查询、密码验证和账号状态判断。
type CredentialService struct {
	// findAccount 由装配方提供账号查询方法，保留请求上下文。
	findAccount func(context.Context, string) (*domain.LoginAccount, error)
	// verifyPassword 在构造时绑定已有比对方法，测试可替换以检查执行顺序。
	verifyPassword func(string, string) (bool, error)
}

// NewCredentialService 接收查询能力并绑定密码验证，不创建数据库或其他资源。
func NewCredentialService(findAccount func(context.Context, string) (*domain.LoginAccount, error)) *CredentialService {
	// 1. 检查查询能力并保存依赖；调用方继续管理连接池。
	if findAccount == nil {
		panic("凭证校验服务需要账号查询方法")
	}
	return &CredentialService{findAccount: findAccount, verifyPassword: VerifyPassword}
}

// Authenticate 检查凭证与账号状态；username 须先通过请求 DTO 的格式校验。
// 任一失败返回 nil/error，成功只返回用户 ID，不创建会话、设备或令牌。
func (credentialService *CredentialService) Authenticate(ctx context.Context, username, rawPassword string) (*domain.VerifiedAccount, error) {
	// 1. 检查请求上下文，再按用户名查询账号。
	if ctx == nil {
		return nil, fmt.Errorf("凭证校验需要上下文")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	account, queryError := credentialService.findAccount(ctx, username)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 2. 为不存在的账号选择固定测试摘要，保留真实查询故障。
	// 这个设计解决的是一个非常隐蔽的安全问题： 时序侧信道攻击
	accountMissing := errors.Is(queryError, domain.ErrAccountNotFound)
	if queryError != nil && !accountMissing {
		return nil, fmt.Errorf("查询凭证账号失败: %w", queryError)
	}
	passwordHash := missingAccountHash
	if !accountMissing {
		if account == nil {
			return nil, domain.ErrInvalidAccountData
		}
		passwordHash = account.PasswordHash
	}

	// 3. 执行密码比对，统一返回凭证错误；计算前后均检查取消。
	matched, verificationError := credentialService.verifyPassword(passwordHash, rawPassword)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if verificationError != nil {
		return nil, fmt.Errorf("校验账号密码失败: %w", verificationError)
	}
	if accountMissing || !matched {
		return nil, domain.ErrInvalidCredentials
	}

	// 4. 密码正确后检查账号状态，只返回最小账号身份。
	if account.ID == 0 {
		return nil, domain.ErrInvalidAccountData
	}
	switch account.Status {
	case "ACTIVE":
		return &domain.VerifiedAccount{ID: account.ID}, nil
	case "DISABLED":
		return nil, domain.ErrAccountDisabled
	default:
		return nil, domain.ErrInvalidAccountData
	}
}
