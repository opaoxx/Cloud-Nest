package service

import (
	"errors"
	"mime"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestBuildDownloadDisposition(t *testing.T) {
	// 1. 对照两种用途、备用名和 UTF-8 编码的具体输出。
	for _, scenario := range []struct {
		label, purpose, name            string
		disposition, fallback, extended string
	}{
		{"ASCII", "download", "Report v2-2026.txt", "attachment", "Report v2-2026.txt", "Report%20v2-2026.txt"},
		{"Chinese", "download", "学习笔记.txt", "attachment", "download.txt", "%E5%AD%A6%E4%B9%A0%E7%AC%94%E8%AE%B0.txt"},
		{"emoji preview", "preview", "🌱 笔记.PNG", "inline", "download.PNG", "%F0%9F%8C%B1%20%E7%AC%94%E8%AE%B0.PNG"},
		{"combining character", "preview", "Cafe\u0301.txt", "inline", "download.txt", "Cafe%CC%81.txt"},
		{"percent apostrophes and semicolon", "download", "100% 与 'A'; +.txt", "attachment", "download.txt", "100%25%20%E4%B8%8E%20%27A%27%3B%20+.txt"},
		{"safe extended token characters", "download", "A!#$&+-.^_`~.txt", "attachment", "download.txt", "A!#$&+-.^_`~.txt"},
		{"punctuation fits UTF-8 encoding", "download", "a(b),[c]{d}=@.txt", "attachment", "download.txt", "a%28b%29%2C%5Bc%5D%7Bd%7D%3D%40.txt"},
		{"dotfile", "download", ".env", "attachment", ".env", ".env"},
		{"non-ASCII dotfile", "download", ".配置", "attachment", "download", ".%E9%85%8D%E7%BD%AE"},
		{"non-ASCII suffix", "preview", "plain.文", "inline", "download", "plain.%E6%96%87"},
		{"trimmed display name", "download", "  file.txt  ", "attachment", "file.txt", "file.txt"},
	} {
		t.Run(scenario.label, func(t *testing.T) {
			value, err := BuildDownloadDisposition(scenario.purpose, scenario.name)
			wantFallback := scenario.disposition + `; filename="` + scenario.fallback + `"`
			if want := wantFallback + "; filename*=UTF-8''" + scenario.extended; err != nil || value != want {
				t.Fatal("Content-Disposition 值错误", value, err)
			}

			// 2. 用标准库从完整头值和仅备用名的头值独立还原文件名。
			kind, params, err := mime.ParseMediaType(value)
			if err != nil || kind != scenario.disposition || params["filename"] != strings.TrimSpace(scenario.name) || len(params) != 1 {
				t.Fatal("标准解析未还原 UTF-8 文件名", kind, params, err)
			}
			_, fallbackParams, err := mime.ParseMediaType(wantFallback)
			if err != nil || fallbackParams["filename"] != scenario.fallback {
				t.Fatal("ASCII 备用名无法解析", fallbackParams, err)
			}
		})
	}
}

func TestBuildDownloadDispositionRejectsInvalidInput(t *testing.T) {
	// 1. 拒绝未支持的用途，输出空值及固定错误。
	for _, purpose := range []string{"", "open", "Preview", " download", "download\n", "下载"} {
		value, err := BuildDownloadDisposition(purpose, "file.txt")
		if value != "" || !errors.Is(err, domain.ErrInvalidDownloadPurpose) {
			t.Fatal("非法用途未被拒绝", value, err)
		}
	}

	// 2. 拒绝控制字符、非法 UTF-8、路径、设备名称和超出展示名长度的输入。
	for _, name := range []string{
		"", "   ", ".", "..", "file.", "dir/name.txt", "dir\\name.txt", `bad"name.txt`,
		"private\r\nname.txt", "private\x00name.txt", "private\x7fname.txt", "private\u0085name.txt", "\xff.txt",
		"con.txt", "LPT¹.txt", strings.Repeat("文", 121), strings.Repeat("🌱", 61),
	} {
		value, err := BuildDownloadDisposition("download", name)
		if value != "" || !errors.Is(err, domain.ErrInvalidEntryName) {
			t.Fatal("非法文件名未返回空值与固定错误", value, err)
		}
	}
}

func TestBuildDownloadDispositionMaximumFilename(t *testing.T) {
	// 1. 对上限长度的 UTF-8 文件名完整往返，响应头始终为可打印 ASCII。
	for _, name := range []string{strings.Repeat("a", 120), strings.Repeat("文", 120), strings.Repeat("🌱", 60)} {
		value, err := BuildDownloadDisposition("download", name)
		if err != nil || strings.ContainsFunc(value, func(character rune) bool { return character < 32 || character > 126 }) {
			t.Fatal("边界文件名生成了无效的头值", err)
		}
		_, params, err := mime.ParseMediaType(value)
		if err != nil || params["filename"] != name {
			t.Fatal("边界文件名在编码往返中丢失", err)
		}
	}
}
