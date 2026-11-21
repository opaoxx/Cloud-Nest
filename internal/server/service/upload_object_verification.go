package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// VerifyUploadObject 从存储重新读取已领取校验租约的对象并核对声明。
// 调用方提供已提交的任务与对应对象记录，后续写回仍须按当前租约复核。
func VerifyUploadObject(
	ctx context.Context,
	task *domain.UploadTask,
	object *domain.StoredObject,
	open func(context.Context, *domain.StoredObject) (io.ReadCloser, error),
) (digest *domain.UploadContentDigest, resultError error) {
	// 1. 核对任务、租约快照、分片计划和暂存对象定位。
	if ctx == nil || open == nil {
		return nil, fmt.Errorf("校验上传对象需要上下文和存储读取方法")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if task == nil || task.ID == 0 || task.UserID == 0 || task.StagedObjectID == 0 {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if task.Status != domain.UploadStatusVerifying || (task.UploadMode != "SINGLE" && task.UploadMode != "MULTIPART") {
		return nil, domain.ErrUploadTaskNotReady
	}
	if task.LeaseVersion == 0 || task.LeaseOwner == nil || *task.LeaseOwner == "" || task.LeaseUntil == nil ||
		!task.LeaseUntil.After(task.UpdatedAt) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	maxSize := domain.MaxSingleUploadSizeBytes
	if task.UploadMode == "MULTIPART" {
		// 使用已保存的计划。int64 上限只保护流长度转换。
		maxSize = math.MaxInt64
		if task.ExpectedSHA256 == nil || task.StorageUploadID == nil || !domain.ValidMultipartUploadID(*task.StorageUploadID) ||
			task.PartSize == nil || task.PartCount == nil || !domain.ValidMultipartUploadLayout(task.FileSize, *task.PartSize, *task.PartCount) {
			return nil, domain.ErrInvalidUploadTaskData
		}
	}
	if task.FileSize > maxSize {
		return nil, domain.ErrUploadFileTooLarge
	}
	if object == nil || object.ID != task.StagedObjectID || object.OwnerUserID != task.UserID ||
		object.Status != domain.StoredObjectStatusVerifying || object.FileSize != task.FileSize {
		return nil, domain.ErrInvalidStoredObjectData
	}
	if task.UploadMode == "MULTIPART" && (object.StorageETag == nil || strings.TrimSpace(*object.StorageETag) == "" ||
		(object.StorageVersionID != nil && strings.TrimSpace(*object.StorageVersionID) == "")) {
		return nil, domain.ErrInvalidStoredObjectData
	}
	declaredSize, expected := task.FileSize, copyOptionalValue(task.ExpectedSHA256)
	taskID, userID, objectID, leaseVersion := task.ID, task.UserID, task.StagedObjectID, task.LeaseVersion

	// 2. 按保存的对象定位打开流，安排退出时关闭。
	stream, openError := open(ctx, object)
	defer func() {
		if stream != nil {
			if closeError := stream.Close(); closeError != nil {
				resultError = errors.Join(resultError, closeError)
			}
		}
		if err := ctx.Err(); err != nil && !errors.Is(resultError, err) {
			resultError = errors.Join(resultError, err)
		}
		if resultError != nil {
			digest = nil
		}
	}()
	if openError != nil {
		return nil, openError
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if stream == nil {
		return nil, domain.ErrUploadStorageUnavailable
	}

	// 3. 按声明读取并记录实际观察量，再核对摘要。
	reader := &uploadVerificationReader{Reader: stream}
	actual, err := readUploadVerificationDigest(ctx, reader, declaredSize, maxSize)
	if err == domain.ErrUploadBodySizeMismatch {
		if reader.readError != nil {
			return nil, errors.Join(err, reader.readError)
		}
		return nil, &domain.UploadVerificationSizeMismatch{
			TaskID: taskID, UserID: userID, StagedObjectID: objectID, LeaseVersion: leaseVersion,
			DeclaredSizeBytes: declaredSize, ObservedSizeBytes: reader.observedSize,
		}
	}
	if err != nil {
		return nil, err
	}
	if err := ValidateUploadContentChecksum(actual.SHA256, expected); err != nil {
		// 失配证据只需要大小和摘要，媒体类型随通过校验的结果交付。
		actual.DetectedMIMEType = ""
		return nil, &domain.UploadVerificationChecksumMismatch{
			TaskID: taskID, UserID: userID, StagedObjectID: objectID, LeaseVersion: leaseVersion,
			ExpectedSHA256: *expected, Actual: *actual,
		}
	}
	return actual, nil
}

// uploadVerificationReader 记录受限读取实际经过的字节和底层读取错误。
type uploadVerificationReader struct {
	io.Reader
	observedSize uint64
	readError    error
}

func (reader *uploadVerificationReader) Read(buffer []byte) (int, error) {
	// 1. 保留额外一字节探测中与数据同时返回的读取故障。
	count, err := reader.Reader.Read(buffer)
	reader.observedSize += uint64(count)
	if err != nil && err != io.EOF {
		reader.readError = err
	}
	return count, err
}
