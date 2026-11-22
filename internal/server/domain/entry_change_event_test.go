package domain

import (
	"crypto/sha256"
	"errors"
	"math"
	"strings"
	"testing"
)

func TestNewFileContentReplacedChangeEvent(t *testing.T) {
	digest := UploadContentDigest{SizeBytes: 11}
	for index := range digest.SHA256 {
		digest.SHA256[index] = 0xab
	}
	for _, sample := range []struct {
		name        string
		entry       FileEntry
		digest      UploadContentDigest
		wantPayload string
	}{
		{
			name: "published contents for the same entry",
			entry: FileEntry{ID: 31, ParentID: new(uint64(13)), Name: "学习笔记 📝.bin",
				EntryType: "FILE", Status: "ACTIVE", Revision: 7, FileSize: 11},
			digest: digest,
			wantPayload: `{"entry_id":"31","parent_id":"13","name":"学习笔记 📝.bin","revision":"7","size_bytes":11,"sha256":"` +
				strings.Repeat("ab", sha256.Size) + `"}`,
		},
		{
			name: "empty file and maximum revision",
			entry: FileEntry{ID: math.MaxUint64, ParentID: new(uint64(math.MaxUint64 - 1)), Name: "empty.bin",
				EntryType: "FILE", Status: "ACTIVE", Revision: math.MaxUint64},
			digest: UploadContentDigest{SHA256: sha256.Sum256(nil)},
			wantPayload: `{"entry_id":"18446744073709551615","parent_id":"18446744073709551614","name":"empty.bin","revision":"18446744073709551615","size_bytes":0,` +
				`"sha256":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}`,
		},
	} {
		t.Run(sample.name, func(t *testing.T) {
			// 1. 核对事件类型、列值及六字段 JSON。
			event, err := NewFileContentReplacedChangeEvent(&sample.entry, sample.digest)
			if err != nil || !event.IsValid() || event.EventType != "FILE_CONTENT_REPLACED" ||
				event.EntryID != sample.entry.ID || event.Revision != sample.entry.Revision ||
				string(event.Payload) != sample.wantPayload {
				t.Fatalf("替换事件不符，实际为 %s %s，错误 %v", event.EventType, event.Payload, err)
			}

			// 2. 同一载荷编码在普通发布时仍交付 FILE_PUBLISHED。
			published, err := NewFilePublishedChangeEvent(&sample.entry, sample.digest.SHA256)
			if err != nil || published.EventType != "FILE_PUBLISHED" || string(published.Payload) != sample.wantPayload {
				t.Fatalf("普通发布的事件格式发生变化，实际为 %s %s，错误 %v", published.EventType, published.Payload, err)
			}
		})
	}
}

func TestNewFileContentReplacedChangeEventRejectsInvalidSnapshot(t *testing.T) {
	digest := UploadContentDigest{SizeBytes: 11}
	entry := FileEntry{ID: 31, ParentID: new(uint64(13)), Name: "note.bin",
		EntryType: "FILE", Status: "ACTIVE", Revision: 7, FileSize: digest.SizeBytes}
	for _, sample := range []struct {
		name   string
		change func(*FileEntry)
	}{
		{name: "missing entry"},
		{name: "zero entry ID", change: func(entry *FileEntry) { entry.ID = 0 }},
		{name: "missing parent", change: func(entry *FileEntry) { entry.ParentID = nil }},
		{name: "zero parent ID", change: func(entry *FileEntry) { entry.ParentID = new(uint64(0)) }},
		{name: "missing name", change: func(entry *FileEntry) { entry.Name = "" }},
		{name: "invalid UTF-8", change: func(entry *FileEntry) { entry.Name = "\xff" }},
		{name: "directory", change: func(entry *FileEntry) { entry.EntryType = "DIR" }},
		{name: "trashed file", change: func(entry *FileEntry) { entry.Status = "TRASHED" }},
		{name: "zero revision", change: func(entry *FileEntry) { entry.Revision = 0 }},
		{name: "initial revision", change: func(entry *FileEntry) { entry.Revision = 1 }},
		{name: "size differs from verification", change: func(entry *FileEntry) { entry.FileSize++ }},
	} {
		t.Run(sample.name, func(t *testing.T) {
			// 1. 每次只改一项事实，失败时不返回半成品事件。
			var snapshot *FileEntry
			if sample.change != nil {
				candidate := entry
				sample.change(&candidate)
				snapshot = &candidate
			}
			event, err := NewFileContentReplacedChangeEvent(snapshot, digest)
			if !errors.Is(err, ErrInvalidChangeEventInput) || event.EntryID != 0 || event.Revision != 0 ||
				event.EventType != "" || event.Payload != nil {
				t.Fatal("异常文件快照未被拒绝", event, err)
			}
		})
	}
}
