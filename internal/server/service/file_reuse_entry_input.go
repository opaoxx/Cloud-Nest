package service

import "github.com/opaoxx/cloud-nest/internal/server/domain"

// BuildFileReuseEntryInput 为已复核的同账号内容准备新文件目录项的写入参数。
// 调用方应在同一账号事务中校验目标路径、锁定并复核对象、选出可用名称和读取当前设备。
func BuildFileReuseEntryInput(
	actor domain.Actor, parentID uint64, resolvedName string, object *domain.StoredObject, sourceDevice *domain.UserDevice,
) (*domain.CreatePublishedFileEntryInput, error) {
	// 1. 核对认证身份和目标目录 ID。
	if actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 {
		return nil, domain.ErrInvalidSessionData
	}
	if parentID == 0 {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 2. 核对已有对象的归属、READY 状态和验证摘要。
	if object == nil || object.ID == 0 || object.OwnerUserID != actor.UserID ||
		object.Status != domain.StoredObjectStatusReady || object.VerifiedSHA256 == nil {
		return nil, domain.ErrInvalidStoredObjectData
	}

	// 3. 核对当前设备并保存设备名称快照。
	if sourceDevice == nil || sourceDevice.ID != actor.DeviceID || sourceDevice.UserID != actor.UserID ||
		!domain.ValidDeviceName(sourceDevice.Name) {
		return nil, domain.ErrInvalidDeviceData
	}
	if sourceDevice.RevokedAt != nil {
		return nil, domain.ErrDeviceRevoked
	}

	// 4. 复用上传发布的名称处理，组装文件写入参数。
	return buildFileEntryWriteInput(parentID, resolvedName, object, sourceDevice)
}
