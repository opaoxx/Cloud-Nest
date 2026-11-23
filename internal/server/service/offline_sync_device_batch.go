package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// OfflineSyncDeviceCandidateError 记录未能确认标记成功的设备和失败原因。
type OfflineSyncDeviceCandidateError struct {
	UserID   uint64
	DeviceID uint64
	Err      error
}

// OfflineSyncDeviceBatchResult 汇总一轮离线设备重建的处理结果。
type OfflineSyncDeviceBatchResult struct {
	Cutoff            time.Time
	Scanned           int
	Marked            int
	StaleCandidates   int
	CandidateErrors   []OfflineSyncDeviceCandidateError
	NextAfterDeviceID uint64
}

// OfflineSyncDeviceBatch 连接数据库截止点、候选分页和单项标记。
type OfflineSyncDeviceBatch struct {
	readCutoff func(context.Context) (time.Time, error)
	list       func(context.Context, time.Time, uint64, int) ([]domain.OfflineSyncDeviceCandidate, error)
	mark       func(context.Context, time.Time, domain.OfflineSyncDeviceCandidate) error
}

// NewOfflineSyncDeviceBatch 装配单轮处理需要的仓储方法。
func NewOfflineSyncDeviceBatch(
	readCutoff func(context.Context) (time.Time, error),
	list func(context.Context, time.Time, uint64, int) ([]domain.OfflineSyncDeviceCandidate, error),
	mark func(context.Context, time.Time, domain.OfflineSyncDeviceCandidate) error,
) *OfflineSyncDeviceBatch {
	if readCutoff == nil || list == nil || mark == nil {
		panic("离线设备重建批次需要截止点、候选分页和单项标记方法")
	}
	return &OfflineSyncDeviceBatch{readCutoff: readCutoff, list: list, mark: mark}
}

// RunOnce 用同一截止点按设备 ID 处理一轮候选，最多读取 limit 条。
// 续扫位置记录最后一项已处理的设备 ID，确认到达末尾后归零。
func (batch *OfflineSyncDeviceBatch) RunOnce(
	ctx context.Context, afterDeviceID uint64, limit, pageSize int,
) (OfflineSyncDeviceBatchResult, error) {
	// 1. 校验上限并读取本轮唯一的截止点。
	summary := OfflineSyncDeviceBatchResult{NextAfterDeviceID: afterDeviceID}
	if ctx == nil || limit < 1 || limit > domain.MaxOfflineSyncDeviceCandidates ||
		pageSize < 1 || pageSize > domain.MaxOfflineSyncDeviceCandidates {
		return summary, fmt.Errorf("离线设备重建批次需要上下文和 1～%d 条的批量及分页上限", domain.MaxOfflineSyncDeviceCandidates)
	}
	if err := ctx.Err(); err != nil {
		return summary, err
	}
	cutoff, err := batch.readCutoff(ctx)
	if err != nil {
		return summary, err
	}
	if cutoff.IsZero() || cutoff.Nanosecond()%int(time.Microsecond) != 0 {
		return summary, domain.ErrInvalidSyncDeviceData
	}
	summary.Cutoff = cutoff.UTC()

	// 2. 按设备 ID 分页，复用同一个截止点。
	for summary.Scanned < limit {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		pageLimit := min(pageSize, limit-summary.Scanned)
		candidates, err := batch.list(ctx, summary.Cutoff, summary.NextAfterDeviceID, pageLimit)
		if err != nil {
			return summary, err
		}
		if !validOfflineSyncDevicePage(candidates, summary.Cutoff, summary.NextAfterDeviceID, pageLimit) {
			return summary, domain.ErrInvalidSyncDeviceData
		}
		summary.Scanned += len(candidates)

		// 3. 逐项提交并汇总结果，记录可续扫的位置。
		for _, candidate := range candidates {
			if err := ctx.Err(); err != nil {
				return summary, err
			}
			err := batch.mark(ctx, summary.Cutoff, candidate)
			if stop := ctx.Err(); stop != nil && errors.Is(err, stop) && !errors.Is(err, domain.ErrUserTransactionCommitUnconfirmed) {
				return summary, stop
			}
			switch {
			case err == nil:
				summary.Marked++
			case errors.Is(err, domain.ErrOfflineSyncDeviceCandidateStale):
				summary.StaleCandidates++
			default:
				summary.CandidateErrors = append(summary.CandidateErrors, OfflineSyncDeviceCandidateError{
					UserID: candidate.UserID, DeviceID: candidate.DeviceID, Err: err,
				})
			}
			summary.NextAfterDeviceID = candidate.DeviceID
		}
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if len(candidates) < pageLimit {
			summary.NextAfterDeviceID = 0
			break
		}
	}
	return summary, ctx.Err()
}

func validOfflineSyncDevicePage(candidates []domain.OfflineSyncDeviceCandidate, cutoff time.Time, afterDeviceID uint64, limit int) bool {
	if len(candidates) > limit {
		return false
	}
	for _, candidate := range candidates {
		if !candidate.IsValid() || candidate.DeviceID <= afterDeviceID || !candidate.LastSeenAt.Before(cutoff) {
			return false
		}
		afterDeviceID = candidate.DeviceID
	}
	return true
}
