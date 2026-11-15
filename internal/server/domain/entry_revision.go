package domain

import "errors"

// ErrEntryRevisionConflict 表示客户端所见版本与当前目录项版本不一致。
var ErrEntryRevisionConflict = errors.New("文件已发生变化，请刷新后重试")

// ErrEntryRevisionExhausted 表示当前版本已达到 uint64 上限，无法保存递增结果。
var ErrEntryRevisionExhausted = errors.New("目录项版本已达上限")

// EntryRevisionConflictError 附带后续冲突响应需要的当前目录项版本。
type EntryRevisionConflictError struct {
	// CurrentRevision 是通过权限与可见性检查后取得的非零当前版本。
	CurrentRevision uint64
}

// Error 返回固定公开提示。
func (conflict *EntryRevisionConflictError) Error() string {
	// 1. 复用固定版本冲突提示。
	return ErrEntryRevisionConflict.Error()
}

// Unwrap 保留统一冲突类别，供 errors.Is 在包装后识别。
func (conflict *EntryRevisionConflictError) Unwrap() error {
	// 1. 返回统一版本冲突错误。
	return ErrEntryRevisionConflict
}
