package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// UploadTempConfig 保存上传临时文件的目录配置。
type UploadTempConfig struct {
	// Directory 是按当前进程工作目录解析后的本机绝对路径。
	Directory string
}

// LoadUploadTempConfig 读取 CLOUDNEST_TEMP_DIR 并解析上传临时目录。
// 默认采用系统临时目录下的 CloudNest/uploads，失败返回零值配置与固定错误。
func LoadUploadTempConfig() (UploadTempConfig, error) {
	// 1. 读取上传临时目录，未设置时采用应用专属默认路径。
	directory := envOrDefault("CLOUDNEST_TEMP_DIR", filepath.Join(os.TempDir(), "CloudNest", "uploads"))

	// 2. 校验路径文本，解析为本机绝对路径。
	if strings.TrimSpace(directory) == "" || !utf8.ValidString(directory) || strings.ContainsFunc(directory, unicode.IsControl) {
		return UploadTempConfig{}, fmt.Errorf("CLOUDNEST_TEMP_DIR 必须是非空、有效 UTF-8 且不含控制字符的目录路径")
	}
	absoluteDirectory, err := filepath.Abs(directory)
	if err != nil {
		return UploadTempConfig{}, fmt.Errorf("CLOUDNEST_TEMP_DIR 无法解析为本机绝对目录路径")
	}

	// 3. 返回目录配置，交给启动装配使用。
	return UploadTempConfig{Directory: absoluteDirectory}, nil
}
