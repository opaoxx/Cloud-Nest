package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LoadOrCreateJWTConfig 优先使用环境密钥，否则读取或首次生成本地签名密钥。
// secretPath 由启动装配方指定，文件内容使用上一轮约定的无填充 Base64URL。
func LoadOrCreateJWTConfig(secretPath string) (JWTConfig, error) {
	// 1. 检查环境覆盖，有明确密钥配置时直接加载。
	if _, configured := os.LookupEnv("CLOUDNEST_JWT_SECRET"); configured {
		return LoadJWTConfig("")
	}
	if strings.TrimSpace(secretPath) == "" {
		return JWTConfig{}, fmt.Errorf("初始化 JWT 配置需要密钥文件路径")
	}

	// 2. 读取已有密钥文件，校验后复用。
	secretText, readError := os.ReadFile(secretPath)
	if readError == nil {
		return LoadJWTConfig(string(secretText))
	}
	if !errors.Is(readError, os.ErrNotExist) {
		return JWTConfig{}, fmt.Errorf("读取 JWT 密钥文件失败: %w", readError)
	}

	// 3. 首次生成随机密钥，校验配置并准备存放目录。
	var randomBytes [32]byte
	rand.Read(randomBytes[:])
	generatedSecret := base64.RawURLEncoding.EncodeToString(randomBytes[:])
	if _, err := LoadJWTConfig(generatedSecret); err != nil {
		return JWTConfig{}, err
	}
	if err := os.MkdirAll(filepath.Dir(secretPath), 0700); err != nil {
		return JWTConfig{}, fmt.Errorf("创建 JWT 密钥目录失败: %w", err)
	}

	// 4. 写完并发布密钥文件，重新读取并返回最终配置。
	if err := publishJWTSecret(secretPath, generatedSecret); err != nil {
		return JWTConfig{}, err
	}
	secretText, readError = os.ReadFile(secretPath)
	if readError != nil {
		return JWTConfig{}, fmt.Errorf("复查 JWT 密钥文件失败: %w", readError)
	}
	return LoadJWTConfig(string(secretText))
}

// publishJWTSecret 将完整临时文件链接到正式路径，已有目标由调用方重新读取。
// 临时文件与目标同目录，硬链接创建成功后保留正式文件名，defer 清理临时文件名。
func publishJWTSecret(secretPath, secretText string) (err error) {
	// 1. 创建独占临时文件，登记关闭与删除，并保留清理错误。
	temporaryFile, err := os.CreateTemp(filepath.Dir(secretPath), ".jwt-secret-*")
	if err != nil {
		return fmt.Errorf("创建 JWT 密钥临时文件失败: %w", err)
	}
	defer func() {
		if closeError := temporaryFile.Close(); closeError != nil && !errors.Is(closeError, os.ErrClosed) {
			err = errors.Join(err, fmt.Errorf("关闭 JWT 密钥临时文件失败: %w", closeError))
		}
		if removeError := os.Remove(temporaryFile.Name()); removeError != nil && !errors.Is(removeError, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("清理 JWT 密钥临时文件失败: %w", removeError))
		}
	}()

	// 2. 写完并同步内容，关闭后再发布正式文件名。
	if _, err := temporaryFile.WriteString(secretText); err != nil {
		return fmt.Errorf("写入 JWT 密钥临时文件失败: %w", err)
	}
	if err := temporaryFile.Sync(); err != nil {
		return fmt.Errorf("同步 JWT 密钥临时文件失败: %w", err)
	}
	if err := temporaryFile.Close(); err != nil {
		return fmt.Errorf("关闭 JWT 密钥临时文件失败: %w", err)
	}

	// 3. 正式路径已存在表示其他初始化者已发布，统一由外层读取最终文件。
	if err := os.Link(temporaryFile.Name(), secretPath); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("发布 JWT 密钥文件失败: %w", err)
	}
	return nil
}
