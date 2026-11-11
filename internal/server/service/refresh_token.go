package service

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// GenerateRefreshToken 生成本次登录使用的刷新令牌原文及其 SHA256 摘要。
// 原文供登录结果交付，摘要供会话入库，两项都按敏感数据处理。
// 沿用 Go 1.27 crypto/rand.Read 的契约，随机源读取失败时标准库终止进程。
func GenerateRefreshToken() (string, [sha256.Size]byte) {
	// 1. 使用 crypto/rand 生成 32 字节安全随机数。
	var randomBytes [32]byte
	rand.Read(randomBytes[:])

	// 2. 编码成便于客户端保存和传输的令牌文本。
	rawToken := base64.RawURLEncoding.EncodeToString(randomBytes[:])

	// 3. 对令牌文本计算 SHA256，返回原文与摘要。
	tokenHash := sha256.Sum256([]byte(rawToken))
	return rawToken, tokenHash
}
