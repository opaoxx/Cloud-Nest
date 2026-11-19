package service

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func publishedFileEntryFixture() (*domain.UploadTask, *domain.StoredObject, *domain.UserDevice) {
	task := &domain.UploadTask{
		ID: 19, UserID: 7, DeviceID: 11, StagedObjectID: 23, TargetParentID: 13,
		RequestedName: "Cafe\u0301.PDF", FileSize: 2048, Status: domain.UploadStatusVerifying, UploadMode: "SINGLE",
	}
	object := &domain.StoredObject{ID: 29, OwnerUserID: task.UserID, FileSize: task.FileSize, Status: domain.StoredObjectStatusReady}
	device := &domain.UserDevice{ID: task.DeviceID, UserID: task.UserID, Name: "  我的电脑  "}
	return task, object, device
}

func TestBuildPublishedFileEntryInput(t *testing.T) {
	for _, scenario := range []struct {
		name, resolved, key, category, extension string
		size                                     uint64
	}{
		{"uses resolved name and reused object", "Cafe\u0301 (2).PDF", "café (2).pdf", "DOCUMENT", "pdf", 2048},
		{"zero bytes and no extension", ".env (1)", ".env (1)", "OTHER", "", 0},
		{"maximum single size", "archive.tar (1).GZ", "archive.tar (1).gz", "ARCHIVE", "gz", domain.MaxSingleUploadSizeBytes},
		{"long extension keeps display name", "a." + strings.Repeat("x", 33), "a." + strings.Repeat("x", 33), "OTHER", "", 2048},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			task, object, device := publishedFileEntryFixture()
			task.FileSize, object.FileSize = scenario.size, scenario.size
			input, err := BuildPublishedFileEntryInput(task, object, scenario.resolved, device)
			if err != nil || input == nil {
				t.Fatal("发布写入输入生成失败", err)
			}
			if input.ParentID != task.TargetParentID || input.Name != scenario.resolved || string(input.NameKey) != scenario.key ||
				input.Category != scenario.category || input.ObjectID != object.ID || input.SizeBytes != scenario.size ||
				input.SourceDeviceID != task.DeviceID || input.SourceDeviceName != device.Name {
				t.Fatalf("发布写入字段不匹配，得到 %+v", input)
			}
			if scenario.extension == "" && input.Extension != nil ||
				scenario.extension != "" && (input.Extension == nil || *input.Extension != scenario.extension) {
				t.Fatal("发布写入扩展名不匹配", input.Extension)
			}
		})
	}
}

func TestBuildPublishedFileEntryInputIndependentSnapshot(t *testing.T) {
	task, object, device := publishedFileEntryFixture()
	task.TargetParentID, object.ID, task.DeviceID, device.ID = math.MaxUint64, math.MaxUint64-1, math.MaxUint64-2, math.MaxUint64-2
	revokedAt := time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)
	device.RevokedAt = &revokedAt
	first, err := BuildPublishedFileEntryInput(task, object, "笔记.PDF", device)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildPublishedFileEntryInput(task, object, "笔记.PDF", device)
	if err != nil {
		t.Fatal(err)
	}
	first.NameKey[0], *first.Extension, device.Name = 0, "changed", "改名后的设备"
	if second.ParentID != math.MaxUint64 || second.ObjectID != math.MaxUint64-1 || second.SourceDeviceID != math.MaxUint64-2 ||
		string(second.NameKey) != "笔记.pdf" || second.Extension == nil || *second.Extension != "pdf" ||
		second.SourceDeviceName != "  我的电脑  " {
		t.Fatal("输入共享了可变字段或丢失完整无符号标识", second)
	}
}

func TestBuildPublishedFileEntryInputErrors(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		change    func(*domain.UploadTask, *domain.StoredObject, *domain.UserDevice)
		wantError error
	}{
		{"missing task ID", func(task *domain.UploadTask, _ *domain.StoredObject, _ *domain.UserDevice) { task.ID = 0 }, domain.ErrInvalidUploadTaskData},
		{"missing parent ID", func(task *domain.UploadTask, _ *domain.StoredObject, _ *domain.UserDevice) { task.TargetParentID = 0 }, domain.ErrInvalidUploadParentID},
		{"wrong task state", func(task *domain.UploadTask, _ *domain.StoredObject, _ *domain.UserDevice) {
			task.Status = domain.UploadStatusCompleted
		}, domain.ErrUploadTaskNotReady},
		{"wrong upload mode", func(task *domain.UploadTask, _ *domain.StoredObject, _ *domain.UserDevice) {
			task.UploadMode = "MULTIPART"
		}, domain.ErrUploadTaskNotReady},
		{"oversize task", func(task *domain.UploadTask, _ *domain.StoredObject, _ *domain.UserDevice) {
			task.FileSize = domain.MaxSingleUploadSizeBytes + 1
		}, domain.ErrUploadFileTooLarge},
		{"foreign object", func(_ *domain.UploadTask, object *domain.StoredObject, _ *domain.UserDevice) { object.OwnerUserID++ }, domain.ErrInvalidStoredObjectData},
		{"unready object", func(_ *domain.UploadTask, object *domain.StoredObject, _ *domain.UserDevice) {
			object.Status = domain.StoredObjectStatusVerifying
		}, domain.ErrInvalidStoredObjectData},
		{"object size mismatch", func(_ *domain.UploadTask, object *domain.StoredObject, _ *domain.UserDevice) { object.FileSize++ }, domain.ErrInvalidStoredObjectData},
		{"wrong device", func(_ *domain.UploadTask, _ *domain.StoredObject, device *domain.UserDevice) { device.ID++ }, domain.ErrInvalidDeviceData},
		{"foreign device", func(_ *domain.UploadTask, _ *domain.StoredObject, device *domain.UserDevice) { device.UserID++ }, domain.ErrInvalidDeviceData},
		{"invalid device name", func(_ *domain.UploadTask, _ *domain.StoredObject, device *domain.UserDevice) { device.Name = " " }, domain.ErrInvalidDeviceData},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			task, object, device := publishedFileEntryFixture()
			scenario.change(task, object, device)
			input, err := BuildPublishedFileEntryInput(task, object, "笔记.PDF", device)
			if input != nil || !errors.Is(err, scenario.wantError) {
				t.Fatalf("生成失败时得到 %+v 和 %v，期望 %v", input, err, scenario.wantError)
			}
		})
	}
	task, object, device := publishedFileEntryFixture()
	for _, scenario := range []struct {
		name      string
		task      *domain.UploadTask
		object    *domain.StoredObject
		device    *domain.UserDevice
		display   string
		wantError error
	}{
		{"nil task", nil, object, device, "笔记.PDF", domain.ErrInvalidUploadTaskData},
		{"nil object", task, nil, device, "笔记.PDF", domain.ErrInvalidStoredObjectData},
		{"nil device", task, object, nil, "笔记.PDF", domain.ErrInvalidDeviceData},
		{"invalid file name", task, object, device, "a/b.PDF", domain.ErrInvalidEntryName},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			input, err := BuildPublishedFileEntryInput(scenario.task, scenario.object, scenario.display, scenario.device)
			if input != nil || !errors.Is(err, scenario.wantError) {
				t.Fatalf("生成失败时得到 %+v 和 %v，期望 %v", input, err, scenario.wantError)
			}
		})
	}
}
