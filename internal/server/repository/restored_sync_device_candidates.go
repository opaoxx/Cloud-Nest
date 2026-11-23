package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

// ListRestoredSyncDeviceCandidates 只读列出一页需要复核的恢复同步记录。
// 整轮以 0 开始，后续页使用上一页最后的设备 ID。调用方使用已核对并停服的恢复目标。
func (store *Store) ListRestoredSyncDeviceCandidates(
	ctx context.Context, afterDeviceID uint64, limit int,
) ([]domain.RestoredSyncDeviceCandidate, error) {
	// 1. 校验页大小和事务外读取入口，设置查询期限。
	if store == nil || store.database == nil || ctx == nil || limit < 1 || limit > domain.MaxRestoredSyncDeviceCandidates {
		return nil, fmt.Errorf("扫描恢复同步设备需要数据入口、上下文和 1～%d 条的扫描上限", domain.MaxRestoredSyncDeviceCandidates)
	}
	if _, inTransaction := store.database.Statement.ConnPool.(gorm.TxCommitter); inTransaction {
		return nil, fmt.Errorf("跨账号扫描恢复同步设备须使用事务外的数据入口")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, err
	}

	// 2. 按设备主键向后读取。左连接保留不能核对归属的行，以便报告错误。
	query := `SELECT restored.user_id, restored.device_id, restored.status, restored.cursor_generation, restored.ack_sequence,
		device.id, account.change_seq
		FROM sync_devices AS restored
		LEFT JOIN user_devices AS device ON device.user_id = restored.user_id AND device.id = restored.device_id
		LEFT JOIN users AS account ON account.id = restored.user_id`
	var arguments []any
	if afterDeviceID != 0 {
		query += " WHERE restored.device_id > ?"
		arguments = append(arguments, afterDeviceID)
	}
	query += " ORDER BY restored.device_id LIMIT ?"
	arguments = append(arguments, limit)
	rows, err := store.database.WithContext(queryContext).Raw(query, arguments...).Rows()
	if err != nil {
		return nil, fmt.Errorf("扫描恢复同步设备失败: %w", err)
	}
	defer rows.Close()

	// 3. 核对归属、确认位置和完整代次，发现异常时整页返回错误。
	candidates := make([]domain.RestoredSyncDeviceCandidate, 0, limit)
	previousDeviceID := afterDeviceID
	for rows.Next() {
		var candidate domain.RestoredSyncDeviceCandidate
		var generation []byte
		var ownedDeviceID, accountHighWater sql.Null[uint64]
		state := &candidate.State
		if err := rows.Scan(&candidate.UserID, &candidate.DeviceID, &state.Status, &generation, &state.AckSequence,
			&ownedDeviceID, &accountHighWater); err != nil {
			return nil, fmt.Errorf("读取恢复同步设备失败: %w", err)
		}
		if candidate.UserID == 0 || candidate.DeviceID <= previousDeviceID ||
			!ownedDeviceID.Valid || ownedDeviceID.V != candidate.DeviceID || !accountHighWater.Valid ||
			state.AckSequence > accountHighWater.V || len(generation) != len(state.Generation) {
			return nil, domain.ErrInvalidSyncDeviceData
		}
		state.Generation = domain.SyncCursorGeneration(generation)
		if !state.IsValid() {
			return nil, domain.ErrInvalidSyncDeviceData
		}
		candidates = append(candidates, candidate)
		previousDeviceID = candidate.DeviceID
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("扫描恢复同步设备失败: %w", err)
	}
	if err := queryContext.Err(); err != nil {
		return nil, err
	}
	return candidates, nil
}
