package service

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestBuildEntryNameMetadataKnownExtensions(t *testing.T) {
	for _, group := range []struct {
		category   string
		extensions []string
	}{
		{"IMAGE", []string{"jpg", "jpeg", "png", "gif", "webp", "avif", "svg"}},
		{"VIDEO", []string{"mp4", "mov", "webm", "mkv"}},
		{"AUDIO", []string{"mp3", "wav", "flac", "m4a", "ogg"}},
		{"ARCHIVE", []string{"zip", "rar", "7z", "tar", "gz"}},
		{"DOCUMENT", []string{"pdf", "md", "txt", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "csv", "json"}},
	} {
		for _, extension := range group.extensions {
			t.Run(extension, func(t *testing.T) {
				for _, suffix := range []string{extension, strings.ToUpper(extension)} {
					metadata, err := BuildEntryNameMetadata("FILE", "  资料."+suffix+"  ")
					if err != nil || metadata == nil || metadata.Name != "资料."+suffix || metadata.Extension == nil || *metadata.Extension != extension || metadata.Category == nil || *metadata.Category != group.category {
						t.Fatal("已知后缀的名称、扩展名或分类错误", err)
					}
				}
				directory, err := BuildEntryNameMetadata("DIR", "资料."+extension)
				if err != nil || directory.Name != "资料."+extension || directory.Extension != nil || directory.Category != nil {
					t.Fatal("文件夹名称被解释为文件扩展名或分类", err)
				}
			})
		}
	}
}

func TestBuildEntryNameMetadataPolicies(t *testing.T) {
	for _, testCase := range []struct {
		name, entryType, raw, display, extension, category string
	}{
		{name: "no dot", entryType: "FILE", raw: "README", display: "README", category: "OTHER"},
		{name: "bare extension word", entryType: "FILE", raw: "pdf", display: "pdf", category: "OTHER"},
		{name: "dotfile", entryType: "FILE", raw: ".env", display: ".env", category: "OTHER"},
		{name: "image-looking dotfile", entryType: "FILE", raw: ".jpg", display: ".jpg", category: "OTHER"},
		{name: "dotfile with extension", entryType: "FILE", raw: ".config.JSON", display: ".config.JSON", extension: "json", category: "DOCUMENT"},
		{name: "compound suffix", entryType: "FILE", raw: "archive.tar.GZ", display: "archive.tar.GZ", extension: "gz", category: "ARCHIVE"},
		{name: "repeated dot", entryType: "FILE", raw: "a..TXT", display: "a..TXT", extension: "txt", category: "DOCUMENT"},
		{name: "unknown suffix", entryType: "FILE", raw: "a.Custom", display: "a.Custom", extension: "custom", category: "OTHER"},
		{name: "unicode suffix", entryType: "FILE", raw: "资料.自定义", display: "资料.自定义", extension: "自定义", category: "OTHER"},
		{name: "unicode lowercase", entryType: "FILE", raw: "资料.ÉXT", display: "资料.ÉXT", extension: "éxt", category: "OTHER"},
		{name: "unicode spelling", entryType: "FILE", raw: "Cafe\u0301.e\u0301", display: "Cafe\u0301.e\u0301", extension: "e\u0301", category: "OTHER"},
		{name: "fullwidth dot", entryType: "FILE", raw: "资料．PDF", display: "资料．PDF", category: "OTHER"},
		{name: "space in suffix", entryType: "FILE", raw: "a. PDF", display: "a. PDF", extension: " pdf", category: "OTHER"},
		{name: "32 ascii", entryType: "FILE", raw: "a." + strings.Repeat("X", 32), display: "a." + strings.Repeat("X", 32), extension: strings.Repeat("x", 32), category: "OTHER"},
		{name: "33 ascii", entryType: "FILE", raw: "a." + strings.Repeat("x", 33), display: "a." + strings.Repeat("x", 33), category: "OTHER"},
		{name: "32 unicode", entryType: "FILE", raw: "a." + strings.Repeat("🌱", 32), display: "a." + strings.Repeat("🌱", 32), extension: strings.Repeat("🌱", 32), category: "OTHER"},
		{name: "33 unicode", entryType: "FILE", raw: "a." + strings.Repeat("🌱", 33), display: "a." + strings.Repeat("🌱", 33), category: "OTHER"},
		{name: "120 utf16 units", entryType: "FILE", raw: strings.Repeat("a", 118) + ".y", display: strings.Repeat("a", 118) + ".y", extension: "y", category: "OTHER"},
		{name: "directory dotfile", entryType: "DIR", raw: ".env", display: ".env"},
		{name: "directory dotted name", entryType: "DIR", raw: "  资料.PDF  ", display: "资料.PDF"},
		{name: "directory long suffix", entryType: "DIR", raw: "a." + strings.Repeat("x", 33), display: "a." + strings.Repeat("x", 33)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			metadata, err := BuildEntryNameMetadata(testCase.entryType, testCase.raw)
			if err != nil || metadata == nil || metadata.Name != testCase.display {
				t.Fatal("名称结果错误", err)
			}
			if testCase.extension == "" {
				if metadata.Extension != nil {
					t.Fatal("无扩展名提示应为空")
				}
			} else if metadata.Extension == nil || *metadata.Extension != testCase.extension {
				t.Fatal("扩展名提示错误")
			}
			if testCase.category == "" {
				if metadata.Category != nil {
					t.Fatal("文件夹分类应为空")
				}
			} else if metadata.Category == nil || *metadata.Category != testCase.category {
				t.Fatal("分类错误")
			}
		})
	}
}

func TestBuildEntryNameMetadataFailures(t *testing.T) {
	for _, entryType := range []string{"FILE", "DIR"} {
		for _, name := range []string{"", "   ", "a.", "CON.txt", "a/b", "a\x00b", "a\nb", "private-name\xff", strings.Repeat("a", 121), strings.Repeat("🌱", 61)} {
			if metadata, err := BuildEntryNameMetadata(entryType, name); metadata != nil || !errors.Is(err, domain.ErrInvalidEntryName) {
				t.Fatal("非法名称未返回空结果及命名错误", entryType, err)
			}
		}
	}
	for _, entryType := range []string{"", "file", "dir", "SYMLINK", "private-type"} {
		for _, name := range []string{"资料.pdf", ""} {
			if metadata, err := BuildEntryNameMetadata(entryType, name); metadata != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatal("未知类型未按记录异常优先处理", err)
			}
		}
	}
}

func TestBuildEntryNameMetadataIndependent(t *testing.T) {
	rawName := "  资料.PDF  "
	first, err := BuildEntryNameMetadata("FILE", rawName)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildEntryNameMetadata("FILE", rawName)
	if err != nil || first == second || first.Extension == second.Extension || first.Category == second.Category {
		t.Fatal("不同调用共享可变元数据", err)
	}
	first.Name, *first.Extension, *first.Category = "本地修改", "changed", "OTHER"
	if second.Name != "资料.PDF" || *second.Extension != "pdf" || *second.Category != "DOCUMENT" || rawName != "  资料.PDF  " {
		t.Fatal("修改结果影响输入或其他结果")
	}
	var workers sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for attempt := 0; attempt < 20; attempt++ {
				metadata, err := BuildEntryNameMetadata("FILE", rawName)
				if err != nil || metadata.Name != "资料.PDF" || metadata.Extension == nil || *metadata.Extension != "pdf" || metadata.Category == nil || *metadata.Category != "DOCUMENT" {
					t.Error("并发元数据计算错误", err)
					return
				}
			}
		}()
	}
	workers.Wait()
}
