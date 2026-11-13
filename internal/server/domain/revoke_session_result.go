package domain

import "errors"

// ErrTargetSessionNotFound 表示撤销目标在当前用户范围内不存在。
// 由服务在目标操作阶段转换，供后续 HTTP 层按资源不存在处理。
var ErrTargetSessionNotFound = errors.New("目标会话不存在")

// RevokeSessionResult 保存指定会话撤销事务确认提交后的内部结果。
type RevokeSessionResult struct {
	// Revoked 为 true 表示目标已处于撤销状态，事务已确认提交。
	Revoked bool
}
