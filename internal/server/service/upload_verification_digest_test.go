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
			if digest == nil || digest.SizeBytes != fixture.declared || digest.SHA256 != sha256.Sum256(fixture.content) {
				t.Fatal("对象大小或摘要不正确", digest)
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
