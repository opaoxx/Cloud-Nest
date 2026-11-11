package requestcontext

import (
	"context"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// actorKey 使用私有类型限定身份存储位置，避免与其他上下文值冲突。
type actorKey struct{}

// WithActor 按值保存授权服务返回的完整身份，保留父上下文的信息与生命周期。
// 调用方须先完成授权。nil 父上下文或缺少身份标识属于调用错误，会触发 panic。
func WithActor(parent context.Context, actor domain.Actor) context.Context {
	// 1. 检查身份标识是否完整。
	if actor.UserID == 0 || actor.SessionID == 0 || actor.DeviceID == 0 {
		panic("请求身份需要有效的用户、会话和设备标识")
	}

	// 2. 使用私有类型键，按值保存到子上下文。
	return context.WithValue(parent, actorKey{}, actor)
}

// Actor 读取已保存身份的副本，缺失或类型不匹配时返回零值和 false。
// 读取身份后，调用方继续遵守上下文的取消与期限。
func Actor(ctx context.Context) (domain.Actor, bool) {
	// 1. 处理空上下文。
	if ctx == nil {
		return domain.Actor{}, false
	}

	// 2. 读取身份副本，返回身份及存在标记。
	actor, exists := ctx.Value(actorKey{}).(domain.Actor)
	return actor, exists
}
