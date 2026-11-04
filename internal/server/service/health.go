// Package service 组织服务规则与依赖调用。
package service

import (
	"context"
	"time"
)

// HealthService 组织数据库和对象存储的只读就绪检查。
type HealthService struct {
	// databaseCheck 检查当前数据库连接，必须遵守传入上下文。
	databaseCheck func(context.Context) error
	// storageCheck 检查当前对象存储桶访问，必须遵守传入上下文。
	storageCheck func(context.Context) error
}

// NewHealthService 保存两项非空检查函数，由启动入口提供实际实现。
func NewHealthService(databaseCheck, storageCheck func(context.Context) error) *HealthService {
	// 1. 保存检查能力，服务层不负责创建或关闭依赖资源。
	return &HealthService{databaseCheck: databaseCheck, storageCheck: storageCheck}
}

// Ready 使用当前请求的上下文，在共享的最多 5 秒内依次检查两项依赖。
// 任一失败、超时或取消都返回 false，全部通过返回 true，不缓存结果。
func (healthService *HealthService) Ready(ctx context.Context) bool {
	// 1. 从当前请求创建共享的检查期限。
	checkContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if checkContext.Err() != nil {
		return false
	}

	// 2. 依次检查 MySQL 和 MinIO，失败时立即返回未就绪。
	if healthService.databaseCheck(checkContext) != nil || checkContext.Err() != nil {
		return false
	}
	if healthService.storageCheck(checkContext) != nil {
		return false
	}

	// 3. 确认检查未超时或取消，返回就绪结果。
	return checkContext.Err() == nil
}
