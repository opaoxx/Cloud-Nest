package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// MaxSyncChangePruneAccountsPerRound 限制一轮实际检查的账号数。
const MaxSyncChangePruneAccountsPerRound = 100

// SyncChangePruneBatchLimits 分别限制一轮、一页和一个账号的工作量。
type SyncChangePruneBatchLimits struct {
	ScanLimit          int
	PageSize           int
	AccountDeleteLimit int
}

// SyncChangePruneAccountError 记录一轮中未能确认裁剪成功的账号和原因。
type SyncChangePruneAccountError struct {
	UserID uint64
	Err    error
}

// SyncChangePruneBatchResult 汇总一轮同步日志裁剪的进度和结果。
type SyncChangePruneBatchResult struct {
	Cutoff          time.Time
	Scanned         int
	PrunedAccounts  int
	DeletedEvents   int
	EmptyAccounts   int
	AccountErrors   []SyncChangePruneAccountError
	NextAfterUserID uint64
}

// SyncChangePruneBatch 连接截止点、候选账号分页和单账号裁剪事务。
type SyncChangePruneBatch struct {
	readCutoff func(context.Context) (time.Time, error)
	list       func(context.Context, time.Time, uint64, int) (domain.SyncChangePruneCandidatePage, error)
	prune      func(context.Context, uint64, time.Time, int) (domain.SyncChangePruneRange, error)
}

// NewSyncChangePruneBatch 装配运行单轮裁剪所需的仓储方法。
func NewSyncChangePruneBatch(
	readCutoff func(context.Context) (time.Time, error),
	list func(context.Context, time.Time, uint64, int) (domain.SyncChangePruneCandidatePage, error),
	prune func(context.Context, uint64, time.Time, int) (domain.SyncChangePruneRange, error),
) *SyncChangePruneBatch {
	if readCutoff == nil || list == nil || prune == nil {
		panic("同步日志裁剪批次需要截止点、候选账号分页和单账号裁剪方法")
	}
	return &SyncChangePruneBatch{readCutoff: readCutoff, list: list, prune: prune}
}

// RunOnce 在一个固定截止点下，按账号扫描预算运行一轮裁剪。
// 已完成的整页可以推进到最后扫描的账号。页内中断时保留最后已处理的账号。
func (batch *SyncChangePruneBatch) RunOnce(
	ctx context.Context, afterUserID uint64, limits SyncChangePruneBatchLimits,
) (SyncChangePruneBatchResult, error) {
	// 1. 校验各项上限，读取一次本轮截止点。
	summary := SyncChangePruneBatchResult{NextAfterUserID: afterUserID}
	if ctx == nil || limits.ScanLimit < 1 || limits.ScanLimit > MaxSyncChangePruneAccountsPerRound ||
		limits.PageSize < 1 || limits.PageSize > domain.MaxSyncChangePruneAccountPageSize ||
		limits.AccountDeleteLimit < 1 || limits.AccountDeleteLimit > domain.MaxSyncChangePruneBatchSize {
		return summary, fmt.Errorf("同步日志裁剪轮次需要上下文和有效的扫描、分页及单账号删除上限")
	}
	if err := ctx.Err(); err != nil {
		return summary, err
	}
	cutoff, err := batch.readCutoff(ctx)
	if err != nil {
		return summary, err
	}
	if cutoff.IsZero() || cutoff.Nanosecond()%int(time.Microsecond) != 0 {
		return summary, fmt.Errorf("同步日志裁剪轮次的截止点无效")
	}
	summary.Cutoff = cutoff.UTC()

	// 2. 按剩余扫描预算读下一页，空候选页也占用扫描预算。
	for summary.Scanned < limits.ScanLimit {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		pageLimit := min(limits.PageSize, limits.ScanLimit-summary.Scanned)
		page, err := batch.list(ctx, summary.Cutoff, summary.NextAfterUserID, pageLimit)
		if err != nil {
			return summary, err
		}
		if !validSyncChangePrunePage(page, summary.NextAfterUserID, pageLimit) {
			return summary, fmt.Errorf("同步日志裁剪候选页无效")
		}
		summary.Scanned += page.ScannedCount

		// 3. 逐账号调用单批事务，单项失败留在汇总中，取消时保留进度。
		for _, userID := range page.UserIDs {
			if err := ctx.Err(); err != nil {
				return summary, err
			}
			deleted, err := batch.prune(ctx, userID, summary.Cutoff, limits.AccountDeleteLimit)
			if stop := ctx.Err(); stop != nil && errors.Is(err, stop) && !errors.Is(err, domain.ErrUserTransactionCommitUnconfirmed) {
				return summary, stop
			}
			if err == nil && !validSyncChangePruneRange(deleted, limits.AccountDeleteLimit) {
				err = fmt.Errorf("同步日志裁剪事务返回无效范围")
			}
			switch {
			case err != nil:
				summary.AccountErrors = append(summary.AccountErrors, SyncChangePruneAccountError{UserID: userID, Err: err})
			case deleted.Count == 0:
				summary.EmptyAccounts++
			default:
				summary.PrunedAccounts++
				summary.DeletedEvents += deleted.Count
			}
			summary.NextAfterUserID = userID
		}
		summary.NextAfterUserID = page.NextAfterUserID
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if page.ScannedCount < pageLimit {
			summary.NextAfterUserID = 0
			break
		}
	}
	return summary, ctx.Err()
}

func validSyncChangePrunePage(page domain.SyncChangePruneCandidatePage, afterUserID uint64, limit int) bool {
	if page.ScannedCount < len(page.UserIDs) || page.ScannedCount > limit || page.NextAfterUserID < afterUserID {
		return false
	}
	if page.ScannedCount == 0 {
		return page.NextAfterUserID == afterUserID
	}
	if page.NextAfterUserID-afterUserID < uint64(page.ScannedCount) {
		return false
	}
	for _, userID := range page.UserIDs {
		if userID <= afterUserID || userID > page.NextAfterUserID {
			return false
		}
		afterUserID = userID
	}
	return true
}

func validSyncChangePruneRange(deleted domain.SyncChangePruneRange, limit int) bool {
	if deleted.Count == 0 {
		return deleted == (domain.SyncChangePruneRange{})
	}
	return deleted.Count > 0 && deleted.Count <= limit && deleted.FirstSequence != 0 &&
		deleted.LastSequence >= deleted.FirstSequence && deleted.LastSequence-deleted.FirstSequence == uint64(deleted.Count-1)
}
