package domain

import (
	"crypto/sha256"
	"errors"
)

// ErrInvalidRefreshToken 表示刷新原文格式无效或当前摘要未命中。
var ErrInvalidRefreshToken = errors.New("刷新凭证无效")

// VerifiedRefreshSession 保存初次刷新凭证校验通过后供事务复查的最小资料。
type VerifiedRefreshSession struct {
	// Session 复用会话标识、用户和设备归属及截止时间。
	Session LoginSession
	// TokenGeneration 是读取时的刷新代次。
	TokenGeneration uint64
	// TokenHash 是刷新原文文本的摘要，按敏感数据处理。
	TokenHash [sha256.Size]byte `json:"-"`
}
