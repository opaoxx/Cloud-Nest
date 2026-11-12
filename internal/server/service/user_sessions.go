package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UserSessionService 读取当前用户的会话列表并标记请求所属会话。
type UserSessionService struct {
	// listSessions 绑定 Repository.ListUserSessions，使用同一调用上下文。
	listSessions func(context.Context, uint64, uint64, int) (*domain.UserSessionPage, error)
}

// NewUserSessionService 绑定已有列表查询，缺少依赖属于装配错误。
func NewUserSessionService(listSessions func(context.Context, uint64, uint64, int) (*domain.UserSessionPage, error)) *UserSessionService {
	// 1. 确认查询依赖存在，保存供每次请求使用。
	if listSessions == nil {
		panic("用户会话服务需要会话列表查询方法")
	}
	return &UserSessionService{listSessions: listSessions}
}

// List 接收已认证 Actor，核对查询结果并返回带 Current 标记的副本。
// beforeSessionID 为 0 时读取首页，pageSize 范围为 1～100。
func (sessionService *UserSessionService) List(ctx context.Context, actor domain.Actor, beforeSessionID uint64, pageSize int) (*domain.UserSessionList, error) {
	// 1. 检查认证身份、分页参数和请求上下文。
	if ctx == nil || actor.UserID == 0 || actor.SessionID == 0 || actor.DeviceID == 0 || pageSize < 1 || pageSize > 100 {
		return nil, fmt.Errorf("读取会话列表需要上下文、完整认证身份和 1～100 的页大小")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 2. 调用会话列表查询，处理错误与取消。
	page, queryError := sessionService.listSessions(ctx, actor.UserID, beforeSessionID, pageSize)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if queryError != nil {
		return nil, fmt.Errorf("读取会话列表失败: %w", queryError)
	}

	// 3. 核对分页、归属及顺序，复制记录并标记当前会话。
	if page == nil || len(page.Sessions) > pageSize || (page.HasMore && len(page.Sessions) != pageSize) {
		return nil, domain.ErrInvalidSessionData
	}
	sessions := make([]domain.UserSessionListItem, 0, len(page.Sessions))
	upperSessionID := beforeSessionID
	for _, session := range page.Sessions {
		current := session.ID == actor.SessionID
		if session.UserID != actor.UserID || session.ID == 0 || session.DeviceID == 0 ||
			(upperSessionID != 0 && session.ID >= upperSessionID) || (current && session.DeviceID != actor.DeviceID) {
			return nil, domain.ErrInvalidSessionData
		}
		upperSessionID = session.ID
		// 三项可空时间逐项复制，返回记录拥有各自的时间对象。
		sessionCopy := session
		if session.LastSeenAt != nil {
			lastSeenAt := *session.LastSeenAt
			sessionCopy.LastSeenAt = &lastSeenAt
		}
		if session.RevokedAt != nil {
			revokedAt := *session.RevokedAt
			sessionCopy.RevokedAt = &revokedAt
		}
		if session.DeviceRevokedAt != nil {
			deviceRevokedAt := *session.DeviceRevokedAt
			sessionCopy.DeviceRevokedAt = &deviceRevokedAt
		}
		sessions = append(sessions, domain.UserSessionListItem{UserSession: sessionCopy, Current: current})
	}

	// 4. 确认请求仍有效，返回独立的列表结果。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &domain.UserSessionList{Sessions: sessions, HasMore: page.HasMore}, nil
}
