package repository

import (
	"context"
	"fmt"
	"time"
)

// ReadOfflineSyncDeviceCutoff 读取本轮离线重建使用的 90 天截止点。
// 调用方在每轮开始时读取一次，整轮扫描及复核复用返回值。
func (store *Store) ReadOfflineSyncDeviceCutoff(ctx context.Context) (time.Time, error) {
	return store.readDatabaseNinetyDayCutoff(ctx, "离线重建截止点")
}

// ReadSyncChangePruneCutoff 读取本轮同步日志裁剪使用的 UTC 微秒截止点。
// 调用方在轮次开始时读取一次，候选扫描及单账号裁剪共用该值。
func (store *Store) ReadSyncChangePruneCutoff(ctx context.Context) (time.Time, error) {
	return store.readDatabaseNinetyDayCutoff(ctx, "同步日志裁剪截止点")
}

func (store *Store) readDatabaseNinetyDayCutoff(ctx context.Context, cutoffName string) (time.Time, error) {
	// 1. 校验仓储入口和查询期限。
	if store == nil || store.database == nil || ctx == nil {
		return time.Time{}, fmt.Errorf("读取%s需要数据入口和上下文", cutoffName)
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return time.Time{}, err
	}

	// 2. 在 MySQL 中一次计算 90 天截止点。
	var cutoff time.Time
	err := store.database.WithContext(queryContext).
		Raw("SELECT DATE_SUB(UTC_TIMESTAMP(6), INTERVAL 90 DAY)").Row().Scan(&cutoff)
	if contextError := queryContext.Err(); contextError != nil {
		return time.Time{}, contextError
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("读取%s失败: %w", cutoffName, err)
	}

	// 3. 核对精度并返回 UTC 时间。
	if cutoff.IsZero() || cutoff.Nanosecond()%int(time.Microsecond) != 0 {
		return time.Time{}, fmt.Errorf("MySQL 返回了无效的%s", cutoffName)
	}
	return cutoff.UTC(), nil
}
