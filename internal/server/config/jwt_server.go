package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// LoadServerJWTConfig 加载服务启动使用的 JWT 配置，环境密钥优先。
// 本地密钥位于用户配置目录的 CloudNest/jwt-secret，沿用已有初始化与复用流程。
func LoadServerJWTConfig() (JWTConfig, error) {
	// 1. 明确配置环境密钥时，直接沿用其校验与优先级规则。
	if _, configured := os.LookupEnv("CLOUDNEST_JWT_SECRET"); configured {
		return LoadOrCreateJWTConfig("")
	}

	// 2. 确定当前服务账号的配置目录，初始化或读取本地密钥。
	configurationDirectory, err := os.UserConfigDir()
	if err != nil {
		return JWTConfig{}, fmt.Errorf("确定 JWT 用户配置目录失败: %w", err)
	}
	return LoadOrCreateJWTConfig(filepath.Join(configurationDirectory, "CloudNest", "jwt-secret"))
}
