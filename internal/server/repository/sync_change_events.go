package repository

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ListSyncChangeEvents 在拉取读视图内按序读取一页原始事件。
// 起点和上界应由调用方在同一 WithSyncChangePullSnapshot 回调内复核。
func (store *Store) ListSyncChangeEvents(ctx context.Context, position, highWaterSequence uint64, pageSize int) ([]domain.SyncChangeEvent, error) {
	// 1. 检查当前读视图的账号绑定、序号范围和页大小。
	if ctx == nil || store == nil || store.syncChangePullUserID == 0 {
		return nil, fmt.Errorf("读取增量事件需要上下文和已绑定账号的拉取读视图")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if position > highWaterSequence {
		return nil, domain.ErrInvalidSyncSnapshotSequence
	}
	if pageSize < 1 || pageSize > 100 {
		return nil, fmt.Errorf("增量事件页大小须在 1～100 条之间")
	}
	events := make([]domain.SyncChangeEvent, 0, pageSize)
	if position == highWaterSequence {
		return events, nil
	}

	// 2. 按账号筛选起点之后、上界以内的事件，升序限量读取。
	rows, err := store.database.WithContext(ctx).Raw(
		"SELECT `sequence`, entry_id, revision, event_type, payload FROM change_events "+
			"WHERE user_id = ? AND `sequence` > ? AND `sequence` <= ? ORDER BY `sequence` ASC LIMIT ?",
		store.syncChangePullUserID, position, highWaterSequence, pageSize,
	).Rows()
	if err != nil {
		return nil, fmt.Errorf("查询增量事件失败: %w", err)
	}
	defer rows.Close()

	// 3. 逐行读取事件和原始载荷，完成整页读取后再返回。
	for rows.Next() {
		var event domain.SyncChangeEvent
		var payload []byte
		if err := rows.Scan(&event.Sequence, &event.EntryID, &event.Revision, &event.EventType, &payload); err != nil {
			return nil, fmt.Errorf("读取增量事件字段失败: %w", err)
		}
		event.Payload = payload
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取增量事件页失败: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return events, nil
}
