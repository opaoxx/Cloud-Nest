package main

import (
	"context"
	"errors"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// uploadCleanupErrorCode 为取消和过期的收尾日志返回固定错误码。
func uploadCleanupErrorCode(err error) string {
	// 1. 识别仍被引用而无法进入回收流程的暂存对象。
	if errors.Is(err, domain.ErrStoredObjectInUse) {
		return "STORED_OBJECT_IN_USE"
	}

	// 2. 其余原因交给上传 worker 共用的错误码映射。
	return uploadWorkerErrorCode(err)
}

// uploadWorkerErrorCode 为上传后台日志映射可识别的错误原因。
func uploadWorkerErrorCode(err error) string {
	// 1. 用 errors.Is 识别直接返回或被包裹的已知原因。
	switch {
	case errors.Is(err, context.Canceled):
		return "CANCELED"
	case errors.Is(err, context.DeadlineExceeded):
		return "DEADLINE_EXCEEDED"
	case errors.Is(err, domain.ErrUploadTaskBusy):
		return "UPLOAD_BUSY"
	case errors.Is(err, domain.ErrUploadTaskNotReady), errors.Is(err, domain.ErrUploadLeaseVersionExhausted):
		return "TASK_NOT_READY"
	case errors.Is(err, domain.ErrUploadTaskNotFound):
		return "UPLOAD_NOT_FOUND"
	case errors.Is(err, domain.ErrUploadObjectMissing):
		return "UPLOAD_OBJECT_MISSING"
	case errors.Is(err, domain.ErrStoredObjectNotFound):
		return "STORED_OBJECT_NOT_FOUND"
	case errors.Is(err, domain.ErrUploadTargetUnavailable):
		return domain.UploadTargetUnavailableCode
	case errors.Is(err, domain.ErrUploadFileTooLarge):
		return "FILE_TOO_LARGE"
	case errors.Is(err, domain.ErrUploadStorageUnavailable):
		return "STORAGE_UNAVAILABLE"
	}

	// 2. 未知原因使用通用错误码，避免把原始细节写入日志。
	return "INTERNAL_ERROR"
}
