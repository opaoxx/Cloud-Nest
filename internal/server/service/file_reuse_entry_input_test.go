package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestBuildFileReuseEntryInput(t *testing.T) {
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	parentID := uint64(13)
	device := &domain.UserDevice{ID: actor.DeviceID, UserID: actor.UserID, Name: "学习电脑"}
	object := &domain.StoredObject{
		ID: 29, OwnerUserID: actor.UserID, Status: domain.StoredObjectStatusReady,
		VerifiedSHA256: new(sha256.Sum256([]byte("synthetic reuse input"))),
		FileSize:       domain.MaxSingleUploadSizeBytes + 1,
	}

	// 1. 模拟原名称已占用，沿用上传发布规则挑选带编号的名称。
	const requestedName = "Cafe\u0301.PDF"
	resolved, err := ResolveUploadPublicationName(t.Context(), actor.UserID, parentID, requestedName,
		func(_ context.Context, userID, folderID uint64, nameKey []byte) (*domain.FileEntry, error) {
			if userID != actor.UserID || folderID != parentID {
				t.Fatal("名称查询超出当前账号和目录")
			}
			switch string(nameKey) {
			case "café.pdf":
				return &domain.FileEntry{ID: 31, UserID: userID, ParentID: &folderID, Status: "ACTIVE"}, nil
			case "café (1).pdf":
				return nil, domain.ErrFileEntryNotFound
			default:
				t.Fatalf("意外的名称键 %q", nameKey)
				return nil, domain.ErrInvalidEntryName
			}
		})
	if err != nil || resolved != "Cafe\u0301 (1).PDF" {
		t.Fatalf("名称选择得到 %q，错误 %v", resolved, err)
	}

	// 2. 确认写入参数保留展示名，沿用判重键，并使用已有对象的完整大小。
	input, err := BuildFileReuseEntryInput(actor, parentID, resolved, object, device)
	if err != nil || input == nil {
		t.Fatal("构造秒传目录项参数失败", err)
	}
	if input.ParentID != parentID || input.Name != resolved || string(input.NameKey) != "café (1).pdf" ||
		input.Extension == nil || *input.Extension != "pdf" || input.Category != "DOCUMENT" || input.MIMEType != nil ||
		input.ObjectID != object.ID || input.SizeBytes != object.FileSize ||
		input.SourceDeviceID != actor.DeviceID || input.SourceDeviceName != device.Name {
		t.Fatalf("写入字段不符，得到 %+v", input)
	}

	// 3. 验证已构造的参数独立于来源记录，并支持无扩展名的零字节文件。
	device.Name = "新设备名"
	object.FileSize = 0
	object.VerifiedSHA256 = new(sha256.Sum256(nil))
	emptyInput, err := BuildFileReuseEntryInput(actor, parentID, ".env", object, device)
	if err != nil || emptyInput == nil || emptyInput.SizeBytes != 0 || emptyInput.Extension != nil || emptyInput.Category != "OTHER" {
		t.Fatalf("零字节文件参数不符，得到 %+v，错误 %v", emptyInput, err)
	}
	if input.SizeBytes != domain.MaxSingleUploadSizeBytes+1 || input.SourceDeviceName != "学习电脑" {
		t.Fatalf("原参数随来源记录发生变化，得到 %+v", input)
	}
}

func TestBuildFileReuseEntryInputErrors(t *testing.T) {
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	noSession := actor
	noSession.SessionID = 0
	object := domain.StoredObject{
		ID: 29, OwnerUserID: actor.UserID, Status: domain.StoredObjectStatusReady,
		VerifiedSHA256: new(sha256.Sum256(nil)),
	}
	foreignObject, deletingObject, unverifiedObject, unidentifiedObject := object, object, object, object
	foreignObject.OwnerUserID++
	deletingObject.Status = domain.StoredObjectStatusDeleting
	unverifiedObject.VerifiedSHA256 = nil
	unidentifiedObject.ID = 0
	device := domain.UserDevice{ID: actor.DeviceID, UserID: actor.UserID, Name: "学习电脑"}
	foreignDevice, otherDevice, invalidDevice, revokedDevice := device, device, device, device
	foreignDevice.UserID++
	otherDevice.ID++
	invalidDevice.Name = " "
	revokedDevice.RevokedAt = new(time.Now())
	const parentID = 13
	const name = "零字节.txt"
	for _, scenario := range []struct {
		label     string
		actor     domain.Actor
		parentID  uint64
		name      string
		object    *domain.StoredObject
		device    *domain.UserDevice
		wantError error
	}{
		{"missing session", noSession, parentID, name, &object, &device, domain.ErrInvalidSessionData},
		{"missing parent", actor, 0, name, &object, &device, domain.ErrInvalidFileEntryData},
		{"foreign object", actor, parentID, name, &foreignObject, &device, domain.ErrInvalidStoredObjectData},
		{"deleting object", actor, parentID, name, &deletingObject, &device, domain.ErrInvalidStoredObjectData},
		{"missing verified digest", actor, parentID, name, &unverifiedObject, &device, domain.ErrInvalidStoredObjectData},
		{"missing object", actor, parentID, name, nil, &device, domain.ErrInvalidStoredObjectData},
		{"missing object ID", actor, parentID, name, &unidentifiedObject, &device, domain.ErrInvalidStoredObjectData},
		{"foreign device", actor, parentID, name, &object, &foreignDevice, domain.ErrInvalidDeviceData},
		{"different device", actor, parentID, name, &object, &otherDevice, domain.ErrInvalidDeviceData},
		{"missing device", actor, parentID, name, &object, nil, domain.ErrInvalidDeviceData},
		{"invalid device name", actor, parentID, name, &object, &invalidDevice, domain.ErrInvalidDeviceData},
		{"revoked device", actor, parentID, name, &object, &revokedDevice, domain.ErrDeviceRevoked},
		{"invalid file name", actor, parentID, "bad/path.txt", &object, &device, domain.ErrInvalidEntryName},
	} {
		t.Run(scenario.label, func(t *testing.T) {
			input, err := BuildFileReuseEntryInput(scenario.actor, scenario.parentID, scenario.name, scenario.object, scenario.device)
			if input != nil || !errors.Is(err, scenario.wantError) {
				t.Fatalf("得到 %+v 和 %v，期望无写入参数及错误 %v", input, err, scenario.wantError)
			}
		})
	}
}
