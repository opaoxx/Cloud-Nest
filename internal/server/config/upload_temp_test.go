package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadUploadTempConfigDefault(t *testing.T) {
	// 1. 暂时移除配置，验证真正未设置时采用的默认路径。
	t.Setenv("CLOUDNEST_TEMP_DIR", "fixture")
	if err := os.Unsetenv("CLOUDNEST_TEMP_DIR"); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(filepath.Join(os.TempDir(), "CloudNest", "uploads"))
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadUploadTempConfig()
	if err != nil || loaded.Directory != want || !filepath.IsAbs(loaded.Directory) {
		t.Fatal("默认上传临时路径错误", loaded, err)
	}
}

func TestLoadUploadTempConfigPaths(t *testing.T) {
	for _, fixture := range []struct {
		name, directory string
	}{
		{"absolute with spaces and Chinese", filepath.Join(t.TempDir(), "上传 文件", "pending")},
		{"relative", filepath.FromSlash("upload-config-fixture/uploads")},
		{"normalize parent", filepath.FromSlash("upload-config-fixture/staging/../uploads")},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			t.Setenv("CLOUDNEST_TEMP_DIR", fixture.directory)
			want, err := filepath.Abs(fixture.directory)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadUploadTempConfig()
			if err != nil || loaded.Directory != want || !filepath.IsAbs(loaded.Directory) {
				t.Fatal("上传目录解析错误", loaded, err)
			}
		})
	}
}

func TestLoadUploadTempConfigInvalid(t *testing.T) {
	for _, directory := range []string{"", "   ", " \t\r\n", "private-fixture\nfolder", "private-fixture\tfolder", "private-fixture\u007ffolder", "private-fixture\u0085folder"} {
		t.Run(strings.ReplaceAll(directory, "private-fixture", "input"), func(t *testing.T) {
			t.Setenv("CLOUDNEST_TEMP_DIR", directory)
			loaded, err := LoadUploadTempConfig()
			if err == nil || loaded != (UploadTempConfig{}) || !strings.Contains(err.Error(), "CLOUDNEST_TEMP_DIR") || strings.Contains(err.Error(), "private-fixture") {
				t.Fatal("非法目录未返回零值和固定错误", loaded, err)
			}
		})
	}
}

func TestLoadUploadTempConfigFilesystemBoundary(t *testing.T) {
	directory := t.TempDir()
	missing := filepath.Join(directory, "uncreated", "uploads")
	t.Setenv("CLOUDNEST_TEMP_DIR", missing)
	loaded, err := LoadUploadTempConfig()
	if err != nil || loaded.Directory != missing {
		t.Fatal("未创建目录的配置读取失败", loaded, err)
	}
	if _, err := os.Stat(filepath.Dir(missing)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("读取配置时创建了目录", err)
	}
	// 已有文件的可用性由后续目录初始化判断，本轮保留其路径和内容。
	blocked := filepath.Join(directory, "ordinary-file")
	if err := os.WriteFile(blocked, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOUDNEST_TEMP_DIR", blocked)
	loaded, err = LoadUploadTempConfig()
	content, readError := os.ReadFile(blocked)
	if err != nil || loaded.Directory != blocked || readError != nil || string(content) != "keep" {
		t.Fatal("路径读取改变已有内容", err, readError)
	}
}
