package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

// ListSyncChangePruneCandidates 只读检查一页账号，筛出最早保留日志已过保留期的候选。
// 首次扫描传入 afterUserID=0。即使本页没有候选，也以返回的扫描位置继续。
func (store *Store) ListSyncChangePruneCandidates(
	ctx context.Context, cutoff time.Time, afterUserID uint64, limit int,
) (domain.SyncChangePruneCandidatePage, error) {
	// 1. 校验固定截止点、页大小和查询上下文。
	var empty domain.SyncChangePruneCandidatePage
	if store == nil || store.database == nil || ctx == nil {
		return empty, fmt.Errorf("扫描同步日志裁剪候选需要数据入口和上下文")
	}
	if limit < 1 || limit > domain.MaxSyncChangePruneAccountPageSize {
		return empty, fmt.Errorf("同步日志裁剪账号页大小须为 1～%d", domain.MaxSyncChangePruneAccountPageSize)
	}
	if cutoff.IsZero() || cutoff.Nanosecond()%int(time.Microsecond) != 0 {
		return empty, fmt.Errorf("扫描同步日志裁剪候选需要有效的微秒精度截止时间")
	}
	if _, inTransaction := store.database.Statement.ConnPool.(gorm.TxCommitter); inTransaction {
		return empty, fmt.Errorf("跨账号扫描同步日志裁剪候选须使用事务外的数据入口")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return empty, err
	}

	// 2. 先限定本页账号，再按日志主键查每个账号的首条。无日志的账号保留在扫描页内。
	const query = "SELECT page.id, COALESCE((" +
		"SELECT retained.created_at FROM change_events AS retained WHERE retained.user_id = page.id " +
		"ORDER BY retained.`sequence` LIMIT 1) < ?, FALSE) " +
		"FROM (SELECT id FROM users WHERE id > ? ORDER BY id LIMIT ?) AS page ORDER BY page.id"
	rows, err := store.database.WithContext(queryContext).Raw(query, cutoff.UTC(), afterUserID, limit).Rows()
	if err != nil {
		return empty, fmt.Errorf("扫描同步日志裁剪候选失败: %w", err)
	}
	defer rows.Close()

	// 3. 游标推进到最后检查的账号，即使这些账号都无需裁剪。
	page := domain.SyncChangePruneCandidatePage{NextAfterUserID: afterUserID}
	for rows.Next() {
		var userID uint64
		var isCandidate bool
		if err := rows.Scan(&userID, &isCandidate); err != nil {
			return empty, fmt.Errorf("读取同步日志裁剪候选失败: %w", err)
		}
		if userID <= page.NextAfterUserID || page.ScannedCount == limit {
			return empty, fmt.Errorf("同步日志裁剪候选分页结果异常")
		}
		if isCandidate {
			page.UserIDs = append(page.UserIDs, userID)
		}
		page.NextAfterUserID = userID
		page.ScannedCount++
	}
	if err := rows.Err(); err != nil {
		return empty, fmt.Errorf("扫描同步日志裁剪候选失败: %w", err)
	}
	if err := queryContext.Err(); err != nil {
		return empty, err
	}
	return page, nil
}
