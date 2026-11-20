package storage

import (
	"context"
	"errors"
	"net"
	"strings"
	"unicode"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// objectReadOptions 核对已保存的位置及版本，供 GET 和 HEAD 共用。
func objectReadOptions(location domain.CreateStagedObjectInput, version *string) (minio.GetObjectOptions, error) {
	// 1. 检查对象键并保留原始位置。
	if !location.IsValid() || strings.TrimSpace(location.ObjectKey) == "" || strings.ContainsFunc(location.ObjectKey, unicode.IsControl) {
		return minio.GetObjectOptions{}, domain.ErrInvalidStoredObjectData
	}

	// 2. 区分未保存版本和明确版本，保留版本原文。
	options := minio.GetObjectOptions{}
	if version != nil {
		if len(*version) == 0 || len(*version) > 255 || strings.ContainsFunc(*version, func(character rune) bool {
			return character < 32 || character > 126
		}) {
			return minio.GetObjectOptions{}, domain.ErrInvalidStoredObjectData
		}
		options.VersionID = *version
	}
	options.Set("Accept-Encoding", "identity")
	return options, nil
}

// matchesObjectReadVersion 保留 MinIO 在版本功能开启前写入的 null 版本语义。
func matchesObjectReadVersion(requested, observed string) bool {
	// 1. 使用不可变键时可以省略版本，明确请求 null 时允许省略响应版本头。
	return requested == "" || observed == requested || (requested == "null" && observed == "")
}

// storageObjectReadError 保留取消和超时，其他存储故障使用调用方的公开类别。
func storageObjectReadError(err, unavailable error) error {
	// 1. 判断取消和超时，隐藏原始存储响应。
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	var networkError net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &networkError) && networkError.Timeout()) {
		return context.DeadlineExceeded
	}
	return unavailable
}
