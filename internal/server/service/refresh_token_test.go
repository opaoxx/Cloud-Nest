package service

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

func checkRefreshToken(t *testing.T, rawToken string, tokenHash [sha256.Size]byte) {
	t.Helper()
	// 1. 核对客户端会收到的文本长度与允许字符。
	if len(rawToken) != 43 {
		t.Fatal("令牌文本须为 43 字节")
	}
	const allowedCharacters = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-"
	for _, character := range rawToken {
		if !strings.ContainsRune(allowedCharacters, character) {
			t.Fatal("令牌包含 Base64URL 范围外的字符")
		}
	}
	randomBytes, err := base64.RawURLEncoding.Strict().DecodeString(rawToken)
	if err != nil || len(randomBytes) != 32 {
		t.Fatal("令牌解码后须为 32 字节")
	}

	// 2. 入库摘要须对应完整令牌文本，确保后续刷新能按收到的文本查询。
	if tokenHash != sha256.Sum256([]byte(rawToken)) {
		t.Fatal("摘要与令牌原文不对应")
	}
	if tokenHash == sha256.Sum256(randomBytes) {
		t.Fatal("摘要错误地取自编码前的随机字节")
	}
}

func TestGenerateRefreshToken(t *testing.T) {
	// 检查连续生成时的格式、摘要及意外复用，失败信息不打印敏感值。
	seen := make(map[string]bool)
	for range 64 {
		rawToken, tokenHash := GenerateRefreshToken()
		checkRefreshToken(t, rawToken, tokenHash)
		if seen[rawToken] {
			t.Fatal("连续调用意外复用了令牌")
		}
		seen[rawToken] = true
	}
}

func TestGenerateRefreshTokenConcurrent(t *testing.T) {
	type generatedToken struct {
		rawToken  string
		tokenHash [sha256.Size]byte
	}
	results := make(chan generatedToken, 32)
	for range cap(results) {
		go func() {
			rawToken, tokenHash := GenerateRefreshToken()
			results <- generatedToken{rawToken: rawToken, tokenHash: tokenHash}
		}()
	}
	seen := make(map[string]bool)
	for range cap(results) {
		result := <-results
		checkRefreshToken(t, result.rawToken, result.tokenHash)
		if seen[result.rawToken] {
			t.Fatal("并发调用意外复用了令牌")
		}
		seen[result.rawToken] = true
	}
}
