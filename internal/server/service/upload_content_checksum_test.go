package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestValidateUploadContentChecksumPolicy(t *testing.T) {
	actual := sha256.Sum256([]byte("abc"))
	zero := [sha256.Size]byte{}
	for _, fixture := range []struct {
		name     string
		actual   [sha256.Size]byte
		expected *[sha256.Size]byte
		want     error
	}{
		{"omitted", actual, nil, nil},
		{"omitted with zero actual", zero, nil, nil},
		{"matching", actual, &actual, nil},
		{"explicit zero differs", actual, &zero, domain.ErrUploadChecksumMismatch},
		{"zero actual differs", zero, &actual, domain.ErrUploadChecksumMismatch},
		{"both zero", zero, &zero, nil},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			originalActual := fixture.actual
			var originalExpected [sha256.Size]byte
			if fixture.expected != nil {
				originalExpected = *fixture.expected
			}
			err := ValidateUploadContentChecksum(fixture.actual, fixture.expected)
			if !errors.Is(err, fixture.want) || fixture.actual != originalActual || (fixture.expected != nil && *fixture.expected != originalExpected) {
				t.Fatal("摘要结果或输入保护错误", err)
			}
		})
	}
	if domain.ErrUploadChecksumMismatch.Error() != "文件校验失败，请重新上传" || !errors.Is(fmt.Errorf("校验失败: %w", domain.ErrUploadChecksumMismatch), domain.ErrUploadChecksumMismatch) {
		t.Fatal("固定提示或包装错误识别失败")
	}
}

func TestValidateUploadContentChecksumEveryByte(t *testing.T) {
	actual := sha256.Sum256([]byte("abc"))
	for position := range actual {
		t.Run(fmt.Sprintf("byte %d", position), func(t *testing.T) {
			expected := actual
			expected[position] ^= 0xff
			originalExpected := expected
			if err := ValidateUploadContentChecksum(actual, &expected); !errors.Is(err, domain.ErrUploadChecksumMismatch) || expected != originalExpected {
				t.Fatal("摘要差异未识别或输入被改写", err)
			}
		})
	}
}

func TestValidateUploadContentChecksumCopiedFiles(t *testing.T) {
	for _, fixture := range []struct {
		name    string
		content []byte
	}{
		{"empty", nil},
		{"abc", []byte("abc")},
		{"UTF8", []byte("学习资料 🌱")},
		{"binary", []byte{0, 0xff, 0xfe, 1, 0x80}},
		{"multiple buffers", bytes.Repeat([]byte{0, 0xff, 1}, 30000)},
		{"SINGLE limit", bytes.Repeat([]byte{0xab}, int(domain.MaxSingleUploadSizeBytes))},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 调用方创建文件，复用实际受限复制取得完整内容摘要。
			file, err := os.Create(filepath.Join(t.TempDir(), "content.part"))
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			result, err := CopyUploadContent(context.Background(), file, bytes.NewReader(fixture.content), uint64(len(fixture.content)))
			if err != nil || result == nil {
				t.Fatal("复制失败", err)
			}
			original := *result
			expected := sha256.Sum256(fixture.content)
			wrong := expected
			wrong[len(wrong)-1] ^= 1
			// 2. 将任务可空摘要交给比较方法，检查成功和失败后的完整结果。
			if ValidateUploadContentChecksum(result.SHA256, nil) != nil || ValidateUploadContentChecksum(result.SHA256, &expected) != nil || !errors.Is(ValidateUploadContentChecksum(result.SHA256, &wrong), domain.ErrUploadChecksumMismatch) || *result != original {
				t.Fatal("复制与摘要比较衔接失败")
			}
			// 3. 调用方继续使用同一文件，确认摘要检查保留文件内容和所有权。
			if err := file.Sync(); err != nil {
				t.Fatal(err)
			}
			if _, err := file.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			content, err := io.ReadAll(file)
			if err != nil || !bytes.Equal(content, fixture.content) || result.SHA256 != sha256.Sum256(content) {
				t.Fatal("文件内容与摘要不一致", err)
			}
		})
	}
}

func TestValidateUploadContentChecksumConcurrent(t *testing.T) {
	actual := sha256.Sum256([]byte("shared immutable expected"))
	expected := actual
	var callers sync.WaitGroup
	for range 16 {
		callers.Go(func() {
			for range 100 {
				if err := ValidateUploadContentChecksum(actual, &expected); err != nil {
					t.Error(err)
				}
				wrong := actual
				wrong[0] ^= 1
				if !errors.Is(ValidateUploadContentChecksum(wrong, &expected), domain.ErrUploadChecksumMismatch) {
					t.Error("并发摘要差异未识别")
				}
			}
		})
	}
	callers.Wait()
	if expected != actual {
		t.Fatal("共享只读摘要被改写")
	}
}
