package security

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// LoginAttemptGate 将 HTTP 连接地址、限流预留和凭证服务连接起来，不写 HTTP 响应。
type LoginAttemptGate struct {
	// beginAttempt 绑定共享限流器的 Begin，不能每次请求创建新的限流器。
	beginAttempt func(netip.Addr, string) (func(bool), time.Duration, error)
	// authenticate 绑定凭证服务方法，不创建设备、会话或令牌。
	authenticate func(context.Context, string, string) (*domain.VerifiedAccount, error)
}

// NewLoginAttemptGate 保存两项必需能力，不创建连接或其他资源。
func NewLoginAttemptGate(
	beginAttempt func(netip.Addr, string) (func(bool), time.Duration, error),
	authenticate func(context.Context, string, string) (*domain.VerifiedAccount, error),
) *LoginAttemptGate {
	// 1. 检查并保存依赖，实际调用在请求通过 DTO 和来源校验之后。
	if beginAttempt == nil || authenticate == nil {
		panic("登录限流适配器需要准入和凭证校验方法")
	}
	return &LoginAttemptGate{beginAttempt: beginAttempt, authenticate: authenticate}
}

// Authenticate 申请名额并执行凭证检查；来源/DTO 校验必须由 Handler 先完成。
// 成功返回身份/0/nil，受限返回 nil/正等待时长/nil，其他失败返回 nil/0/error。
// 适配器负责兜底释放，调用方不再直接操作完成回调。
func (gate *LoginAttemptGate) Authenticate(request *http.Request, username, rawPassword string) (*domain.VerifiedAccount, time.Duration, error) {
	// 1. 检查请求上下文，解析可信连接 IP。
	if request == nil {
		return nil, 0, fmt.Errorf("登录限流适配器需要 HTTP 请求")
	}
	ctx := request.Context()
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	peerIP, err := DirectPeerIP(request)
	if err != nil {
		return nil, 0, err
	}

	// 2. 在凭证查询前申请限流名额，受限直接返回等待建议。
	complete, retryAfter, err := gate.beginAttempt(peerIP, username)
	if err != nil {
		return nil, 0, err
	}
	if complete == nil {
		if retryAfter <= 0 {
			return nil, 0, fmt.Errorf("限流器未返回有效的准入结果")
		}
		return nil, retryAfter, nil
	}

	// 3. 登记兜底释放，再调用凭证服务；取消后不返回身份。
	defer complete(false)
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	identity, credentialError := gate.authenticate(ctx, username, rawPassword)
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}

	// 4. 只把错误凭证结算为失败，其他结果释放预留。
	complete(errors.Is(credentialError, domain.ErrInvalidCredentials))
	if credentialError != nil {
		return nil, 0, credentialError
	}
	if identity == nil || identity.ID == 0 {
		return nil, 0, domain.ErrInvalidAccountData
	}
	return identity, 0, nil
}
