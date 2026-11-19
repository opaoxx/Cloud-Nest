package service

import "github.com/opaoxx/cloud-nest/internal/server/domain"

// BuildPublishedFileEntryInput 将已复核的上传任务、发布对象、名称及设备整理为文件写入输入。
// 调用方在发布事务中准备这些记录，仓储写入时继续复核当前持久化事实。
func BuildPublishedFileEntryInput(
	task *domain.UploadTask,
	object *domain.StoredObject,
	resolvedName string,
	sourceDevice *domain.UserDevice,
) (*domain.CreatePublishedFileEntryInput, error) {
	// 1. 核对任务与发布对象的归属、状态和大小。
	if task == nil || task.ID == 0 || task.UserID == 0 || task.DeviceID == 0 {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if task.TargetParentID == 0 {
		return nil, domain.ErrInvalidUploadParentID
	}
	if task.Status != domain.UploadStatusVerifying || task.UploadMode != "SINGLE" {
		return nil, domain.ErrUploadTaskNotReady
	}
	if task.FileSize > domain.MaxSingleUploadSizeBytes {
		return nil, domain.ErrUploadFileTooLarge
	}
	if object == nil || object.ID == 0 || object.OwnerUserID != task.UserID ||
		object.Status != domain.StoredObjectStatusReady || object.FileSize != task.FileSize {
		return nil, domain.ErrInvalidStoredObjectData
	}

	// 2. 核对来源设备与任务的对应关系。
	if sourceDevice == nil || sourceDevice.ID != task.DeviceID || sourceDevice.UserID != task.UserID ||
		!domain.ValidDeviceName(sourceDevice.Name) {
		return nil, domain.ErrInvalidDeviceData
	}

	// 3. 派生名称键、扩展名和分类，组装写入输入。
	metadata, err := BuildEntryNameMetadata("FILE", resolvedName)
	if err != nil {
		return nil, err
	}
	nameKey, err := BuildEntryNameKey(metadata.Name)
	if err != nil {
		return nil, err
	}
	return &domain.CreatePublishedFileEntryInput{
		ParentID:         task.TargetParentID,
		Name:             metadata.Name,
		NameKey:          nameKey,
		Category:         *metadata.Category,
		Extension:        metadata.Extension,
		ObjectID:         object.ID,
		SizeBytes:        object.FileSize,
		SourceDeviceID:   sourceDevice.ID,
		SourceDeviceName: sourceDevice.Name,
	}, nil
}
