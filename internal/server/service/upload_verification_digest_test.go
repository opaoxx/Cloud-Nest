package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"testing"
	"testing/iotest"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestReadUploadVerificationDigest(t *testing.T) {
	binary := []byte{0, 0xff, 0x80, 0x01}
	for _, fixture := range []struct {
		name     string
		content  []byte
		declared uint64
		readErr  error
		wantErr  error
		wantRead int
	}{
		{"empty object", nil, 0, nil, nil, 0},
		{"binary object", binary, 4, nil, nil, 4},
		{"short object", binary, 5, nil, domain.ErrUploadBodySizeMismatch, 4},
		{"long object", binary, 1, nil, domain.ErrUploadBodySizeMismatch, 2},
		{"storage read fails", binary, 5, domain.ErrUploadStorageUnavailable, domain.ErrUploadStorageUnavailable, 4},
		{"storage read fails during end probe", binary, 4, domain.ErrUploadStorageUnavailable, domain.ErrUploadStorageUnavailable, 4},
		{"declared size too large", binary, domain.MaxSingleUploadSizeBytes + 1, nil, domain.ErrUploadFileTooLarge, 0},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			var reader io.Reader = iotest.OneByteReader(bytes.NewReader(fixture.content))
			if fixture.readErr != nil {
				reader = io.MultiReader(reader, iotest.ErrReader(fixture.readErr))
			}
			source := &uploadCopySource{reader: reader}
			digest, err := ReadUploadVerificationDigest(t.Context(), source, fixture.declared)
			if !errors.Is(err, fixture.wantErr) || source.bytesRead != fixture.wantRead || source.closes != 0 || source.shortcut {
				t.Fatal("校验返回错误、读取量或流所有权不符", err, source.bytesRead)
			}
			if fixture.wantErr != nil {
				if digest != nil {
					t.Fatal("失败仍返回了摘要", digest)
				}
				return
			}
			if digest == nil || digest.SizeBytes != fixture.declared || digest.SHA256 != sha256.Sum256(fixture.content) ||
				digest.DetectedMIMEType != "" {
				t.Fatal("对象大小、摘要或媒体类型不正确", digest)
			}
		})
	}
}

func TestReadUploadVerificationDigestMediaType(t *testing.T) {
	pngHeader := []byte("\x89PNG\r\n\x1a\n")
	atTextLimit := bytes.Repeat([]byte{'a'}, maxTextPreviewBytes)
	for _, fixture := range []struct {
		name    string
		content []byte
		want    string
	}{
		{"png signature", pngHeader, "image/png"},
		{"jpeg signature", []byte("\xff\xd8\xff\xe0"), "image/jpeg"},
		{"gif signature", []byte("GIF89a"), "image/gif"},
		{"webp signature", []byte("RIFF\x00\x00\x00\x00WEBPVP8 "), "image/webp"},
		{"image larger than text limit", append(bytes.Clone(pngHeader), atTextLimit...), "image/png"},
		{"multibyte UTF-8 text", []byte("学习记录\tCloudNest\r\n"), "text/plain"},
		{"UTF-8 BOM", []byte("\xef\xbb\xbf学习记录"), "text/plain"},
		{"text at limit", atTextLimit, "text/plain"},
		{"text over limit", append(bytes.Clone(atTextLimit), 'a'), ""},
		{"UTF-8 rune split by signature boundary", append(bytes.Repeat([]byte{'a'}, uploadMIMESampleBytes-1), []byte("云")...), "text/plain"},
		{"invalid UTF-8 beyond signature", append(bytes.Repeat([]byte{'a'}, uploadMIMESampleBytes), 0xff), ""},
		{"NUL beyond signature", append(bytes.Repeat([]byte{'a'}, uploadMIMESampleBytes), 0), ""},
		{"Unicode control code", []byte("sample\u0085text"), ""},
		{"UTF-16 text", []byte{0xff, 0xfe, 'a', 0}, ""},
		{"HTML", []byte("<!DOCTYPE html><html></html>"), ""},
		{"PDF", []byte("%PDF-1.7"), ""},
		{"partial PNG signature", pngHeader[:6], ""},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 以分段读取验证文件头和跨读取边界的多字节文本。
			var reader io.Reader = bytes.NewReader(fixture.content)
			if len(fixture.content) < 1024 {
				reader = iotest.OneByteReader(reader)
			}
			source := &uploadCopySource{reader: reader}
			declaredSize := uint64(len(fixture.content))
			digest, err := ReadUploadVerificationDigest(t.Context(), source, declaredSize)

			// 2. 核对实际读完的字节、完整摘要及可选 MIME。
			if err != nil || digest == nil || digest.DetectedMIMEType != fixture.want ||
				digest.SizeBytes != declaredSize || digest.SHA256 != sha256.Sum256(fixture.content) ||
				source.bytesRead != len(fixture.content) || source.closes != 0 {
				t.Fatal("流式媒体类型识别结果不符", digest, err, source.bytesRead)
			}
		})
	}
}

func TestReadUploadVerificationDigestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	source := &uploadCopySource{reader: bytes.NewReader([]byte("stored")), afterRead: func(int) { cancel() }}
	digest, err := ReadUploadVerificationDigest(ctx, source, 6)
	if digest != nil || !errors.Is(err, context.Canceled) || source.reads != 1 || source.closes != 0 {
		t.Fatal("取消后仍返回摘要、继续读取或关闭了调用方资源", digest, err)
	}
}
