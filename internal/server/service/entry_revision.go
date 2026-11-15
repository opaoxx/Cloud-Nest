package service

import (
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ValidateEntryRevision 核对客户端预期版本与当前目录项版本。
// 调用方先检查身份、目标归属与可见性，并在持锁事务中比对及后续更新。
func ValidateEntryRevision(currentRevision, expectedRevision uint64) error {
	// 1. 检查客户端预期版本是否非零。
	if expectedRevision == 0 {
		return fmt.Errorf("校验目录项版本需要非零预期版本")
	}

	// 2. 检查当前目录项版本是否有效。
	if currentRevision == 0 {
		return domain.ErrInvalidFileEntryData
	}

	// 3. 比对版本，不一致时返回带当前版本的冲突错误。
	if currentRevision != expectedRevision {
		return &domain.EntryRevisionConflictError{CurrentRevision: currentRevision}
	}
	return nil
}
