package service

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestResolveDownloadContentTypeDownload(t *testing.T) {
	// 1. 已知媒体类型按契约返回小写且无参数的 MIME。
	longest := "application/" + strings.Repeat("a", 115)
	for _, scenario := range []struct{ name, stored, want string }{
		{"normalized type", "  APPLICATION/VND.CloudNest+JSON  ", "application/vnd.cloudnest+json"},
		{"valid parameters", `TEXT/PLAIN; charset="utf-8"`, "text/plain"},
		{"recognized outside preview", "application/pdf", "application/pdf"},
		{"attachment SVG", "image/svg+xml", "image/svg+xml"},
		{"longest stored type", longest, longest},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			got, err := ResolveDownloadContentType("download", &scenario.stored, math.MaxUint64)
			if err != nil || got != scenario.want {
				t.Fatal("普通下载媒体类型错误", got, err)
			}
		})
	}

	// 2. 缺失或不可信的记录降级为一般二进制类型，预览拒绝同样的输入。
	for _, scenario := range []struct {
		name   string
		stored *string
	}{
		{"missing", nil},
		{"empty", new("")},
		{"blank", new("  ")},
		{"without subtype", new("text")},
		{"wildcard", new("image/*")},
		{"multiple types", new("text/plain, image/png")},
		{"invalid parameters", new("text/plain; charset")},
		{"injection", new("text/plain\r\nX-Extra: true")},
		{"trailing control", new("image/png\t")},
		{"unicode token", new("image/图")},
		{"invalid UTF-8", new("text/plain\xff")},
		{"invalid token for contract", new("text/plain%")},
		{"too long", new(longest + "a")},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if got, err := ResolveDownloadContentType("download", scenario.stored, 1); err != nil || got != "application/octet-stream" {
				t.Fatal("无效媒体类型未回退到二进制类型", got, err)
			}
			if got, err := ResolveDownloadContentType("preview", scenario.stored, 1); !errors.Is(err, domain.ErrUnsupportedPreview) || got != "" {
				t.Fatal("预览接受了不可信媒体类型", got, err)
			}
		})
	}
}

func TestResolveDownloadContentTypePreview(t *testing.T) {
	// 1. 只允许首期名单中的图片与 2 MiB 以内的纯文本。
	for _, scenario := range []struct {
		name, stored, want string
		size               uint64
	}{
		{"PNG", "image/png", "image/png", 4},
		{"JPEG", "image/JPEG", "image/jpeg", 4},
		{"GIF", "image/gif", "image/gif", 2097153},
		{"WebP", "image/webp", "image/webp", 4},
		{"empty text", "text/plain", "text/plain", 0},
		{"text below limit", "text/plain", "text/plain", 2097151},
		{"text at limit", "TEXT/PLAIN; charset=utf-8", "text/plain", 2097152},
		{"text above limit", "text/plain", "", 2097153},
		{"maximum unsigned size", "text/plain", "", math.MaxUint64},
		{"unverified bytes", "application/octet-stream", "", 4},
		{"HTML", "text/html", "", 4},
		{"SVG", "image/svg+xml", "", 4},
		{"PDF", "application/pdf", "", 4},
		{"video", "video/mp4", "", 4},
		{"Markdown", "text/markdown", "", 4},
		{"unsupported raster image", "image/bmp", "", 4},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			contentType, err := ResolveDownloadContentType("preview", &scenario.stored, scenario.size)
			var wantError error
			if scenario.want == "" {
				wantError = domain.ErrUnsupportedPreview
			}
			if contentType != scenario.want || !errors.Is(err, wantError) {
				t.Fatal("预览类型、大小或错误不符合契约", contentType, err)
			}
		})
	}
}

func TestResolveDownloadContentTypeRejectsInvalidPurpose(t *testing.T) {
	for _, purpose := range []string{"", "open", "Preview", " download"} {
		if got, err := ResolveDownloadContentType(purpose, new("image/png"), 1); got != "" || !errors.Is(err, domain.ErrInvalidDownloadPurpose) {
			t.Fatal("无效用途应返回空媒体类型和固定错误", got, err)
		}
	}
}
