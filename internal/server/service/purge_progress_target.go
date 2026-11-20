package service

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// PurgeProgressTargetReader 按已认证用户读取进度目标的持久状态。
type PurgeProgressTargetReader interface {
	FindUserPurgeProgressTarget(ctx context.Context, userID, entryID uint64) (*domain.PurgeProgressTarget, error)
}

// ReadPurgeProgressTarget 读取并判断目标是否具备进度查询资格。
// 调用方应在同一只读快照内继续读取剩余项。
func ReadPurgeProgressTarget(ctx context.Context, actor domain.Actor, entryID uint64, reader PurgeProgressTargetReader) (*domain.PurgeProgressTarget, error) {
	// 1. 核对认证身份和目标 ID，按当前用户读取目标。
	if ctx == nil || reader == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || entryID == 0 {
		return nil, fmt.Errorf("读取清理进度目标需要上下文、读取能力、完整认证身份和非零目标 ID")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	target, readError := reader.FindUserPurgeProgressTarget(ctx, actor.UserID, entryID)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if readError != nil {
		return nil, fmt.Errorf("读取清理进度目标失败: %w", readError)
	}
	if target == nil || target.UserID != actor.UserID || target.ID != entryID {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 2. 按目标状态和开始时间判断是否可查。
	if target.ParentID == nil {
		return nil, domain.ErrFileEntryNotFound
	}
	if *target.ParentID == 0 || *target.ParentID == target.ID {
		return nil, domain.ErrInvalidFileEntryData
	}
	switch target.Status {
	case "ACTIVE", "TRASHED":
		return nil, domain.ErrFileEntryNotFound
	case "PURGING", "PURGED":
		if target.Status == "PURGED" && target.PurgeStartedAt == nil {
			return nil, domain.ErrFileEntryNotFound
		}
	default:
		return nil, domain.ErrInvalidFileEntryData
	}

	// 3. 校验清理时间，返回独立的查询结果。
	if !validPurgeProgressTime(target.PurgeStartedAt) {
		return nil, domain.ErrInvalidFileEntryData
	}
	if target.Status == "PURGING" {
		if target.PurgedAt != nil {
			return nil, domain.ErrInvalidFileEntryData
		}
	} else if !validPurgeProgressTime(target.PurgedAt) || target.PurgedAt.Before(*target.PurgeStartedAt) {
		return nil, domain.ErrInvalidFileEntryData
	}
	result := *target
	result.ParentID = copyOptionalValue(target.ParentID)
	result.PurgeStartedAt = copyOptionalValue(target.PurgeStartedAt)
	result.PurgedAt = copyOptionalValue(target.PurgedAt)
	return &result, nil
}

// validPurgeProgressTime 核对 MySQL DATETIME(6) 对应的 UTC 年份范围和微秒精度。
func validPurgeProgressTime(timestamp *time.Time) bool {
	// 1. 空值由调用方区分用途，此处只接受有效的数据库时间。
	if timestamp == nil {
		return false
	}
	year := timestamp.UTC().Year()
	return year >= 1000 && year <= 9999 && timestamp.Nanosecond()%1000 == 0
}
