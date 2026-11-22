package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

// AppendUserChangeEvent 在持有用户行锁的事务回调中追加一条已核对归属的变更。
// 调用方将错误交回外层事务，返回的序号在外层事务成功提交后生效。
func (store *Store) AppendUserChangeEvent(ctx context.Context, input domain.ChangeEventInput) (uint64, error) {
	// 1. 核对账号事务和事件字段。
	if ctx == nil || store == nil || store.database == nil || store.loginUserID == 0 {
		return 0, fmt.Errorf("追加变更事件需要持锁账号事务和上下文")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return 0, fmt.Errorf("追加变更事件必须使用账号事务 Store")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !input.IsValid() {
		return 0, domain.ErrInvalidChangeEventInput
	}

	// 2. 读取账号当前序号，检查上限后推进一位。
	var currentSequence uint64
	transaction := store.database.WithContext(ctx)
	err := transaction.Raw("SELECT change_seq FROM users WHERE id = ? FOR UPDATE", store.loginUserID).Row().Scan(&currentSequence)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, domain.ErrAccountNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("读取账号变更序号失败: %w", err)
	}
	if currentSequence == math.MaxUint64 {
		return 0, domain.ErrChangeSequenceExhausted
	}
	nextSequence := currentSequence + 1
	advanced := transaction.Exec("UPDATE users SET change_seq = ? WHERE id = ? AND change_seq = ?", nextSequence, store.loginUserID, currentSequence)
	if advanced.Error != nil {
		return 0, fmt.Errorf("推进账号变更序号失败: %w", advanced.Error)
	}
	if advanced.RowsAffected != 1 {
		return 0, domain.ErrInvalidAccountData
	}

	// 3. 写入同序号的事件，由外层事务统一提交或回滚。
	inserted := transaction.Exec(
		"INSERT INTO change_events (user_id, `sequence`, entry_id, revision, event_type, payload, created_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, UTC_TIMESTAMP(6))",
		store.loginUserID, nextSequence, input.EntryID, input.Revision, input.EventType, string(input.Payload))
	if inserted.Error != nil {
		return 0, fmt.Errorf("保存账号变更事件失败: %w", inserted.Error)
	}
	if inserted.RowsAffected != 1 {
		return 0, fmt.Errorf("追加变更事件应恰好写入一行，实际 %d 行", inserted.RowsAffected)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return nextSequence, nil
}
