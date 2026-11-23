package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// selectSyncChangePrunePrefix 在所属账号事务内只读选出本批可裁剪的连续范围。
func (store *Store) selectSyncChangePrunePrefix(ctx context.Context, cutoff time.Time, limit int) (domain.SyncChangePruneRange, error) {
	// 1. 校验截止时间和批量上限，在账号锁内读取确认上界。
	var empty domain.SyncChangePruneRange
	if cutoff.IsZero() || cutoff.Nanosecond()%int(time.Microsecond) != 0 {
		return empty, fmt.Errorf("筛选日志裁剪范围需要有效的微秒精度截止时间")
	}
	if limit < 1 || limit > domain.MaxSyncChangePruneBatchSize {
		return empty, fmt.Errorf("日志裁剪筛选上限须为 1～%d", domain.MaxSyncChangePruneBatchSize)
	}
	ackLimit, err := store.readSyncChangePruneAckLimit(ctx)
	if err != nil {
		return empty, err
	}

	// 2. 使用账号和序号索引按顺序读取。保留时间条件在逐条检查时应用，以免跳过近期日志。
	const query = "SELECT `sequence`, created_at FROM change_events " +
		"WHERE user_id = ? AND `sequence` <= ? ORDER BY `sequence` LIMIT ?"
	rows, err := store.database.WithContext(ctx).Raw(query, store.loginUserID, ackLimit, limit).Rows()
	if err != nil {
		return empty, fmt.Errorf("读取日志裁剪候选失败: %w", err)
	}
	defer rows.Close()

	// 3. 遇到保留期边界就停止。发现已检查范围存在断档时，交付空结果和错误。
	var prefix domain.SyncChangePruneRange
	stoppedAtCutoff := false
	for rows.Next() {
		var sequence uint64
		var createdAt time.Time
		if err := rows.Scan(&sequence, &createdAt); err != nil {
			return empty, fmt.Errorf("读取日志裁剪候选行失败: %w", err)
		}
		if sequence == 0 || (prefix.Count > 0 && sequence-1 != prefix.LastSequence) {
			return empty, fmt.Errorf("日志裁剪候选序号不连续: %w", domain.ErrInvalidSyncSnapshotSequence)
		}
		if createdAt.IsZero() {
			return empty, fmt.Errorf("日志裁剪候选缺少有效创建时间")
		}
		if !createdAt.Before(cutoff) {
			stoppedAtCutoff = true
			break
		}
		if prefix.Count == 0 {
			prefix.FirstSequence = sequence
		}
		prefix.LastSequence = sequence
		prefix.Count++
	}
	if err := rows.Err(); err != nil {
		return empty, fmt.Errorf("读取日志裁剪候选失败: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if !stoppedAtCutoff && prefix.Count > 0 && prefix.Count < limit && prefix.LastSequence < ackLimit {
		return empty, fmt.Errorf("日志裁剪候选提前读尽: %w", domain.ErrInvalidSyncSnapshotSequence)
	}
	return prefix, nil
}
