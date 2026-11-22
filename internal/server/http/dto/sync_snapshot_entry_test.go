package dto

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestNewSyncSnapshotEntryJSON(t *testing.T) {
	// 1. 分别准备文件、零字节文件和目录的公开 JSON 预期。
	fileDigest := [sha256.Size]byte{0xab, 0x0f, 0xff}
	emptyDigest := sha256.Sum256(nil)
	for _, testCase := range []struct {
		name  string
		entry domain.SyncSnapshotEntry
		want  string
	}{
		{
			name: "full identifiers and safe byte boundary",
			entry: domain.SyncSnapshotEntry{
				ID: math.MaxUint64, ParentID: math.MaxUint64 - 1, Name: "学习 e\u0301.txt", EntryType: "FILE",
				Revision: math.MaxUint64, FileSize: maxJSONSafeInteger, VerifiedSHA256: &fileDigest,
			},
			want: `{"entry_id":"18446744073709551615","parent_id":"18446744073709551614","name":"学习 é.txt","entry_type":"FILE","revision":"18446744073709551615","size_bytes":9007199254740991,"sha256":"ab0fff` + strings.Repeat("00", 29) + `"}`,
		},
		{
			name: "empty file keeps its checksum",
			entry: domain.SyncSnapshotEntry{
				ID: 13, ParentID: 11, Name: "空文件.txt", EntryType: "FILE", Revision: 1, VerifiedSHA256: &emptyDigest,
			},
			want: `{"entry_id":"13","parent_id":"11","name":"空文件.txt","entry_type":"FILE","revision":"1","size_bytes":0,"sha256":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}`,
		},
		{
			name:  "directory emits explicit null",
			entry: domain.SyncSnapshotEntry{ID: 12, ParentID: 11, Name: "目录", EntryType: "DIR", Revision: 2},
			want:  `{"entry_id":"12","parent_id":"11","name":"目录","entry_type":"DIR","revision":"2","size_bytes":0,"sha256":null}`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 2. 编码实际公开结构，核对字段集合、各值类型和完整数值。
			data, err := NewSyncSnapshotEntry(testCase.entry)
			if err != nil {
				t.Fatal("转换快照条目失败", err)
			}
			encoded, err := json.Marshal(data)
			if err != nil {
				t.Fatal("编码快照条目失败", err)
			}
			var actual, want map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &actual); err != nil {
				t.Fatal("解码实际快照条目失败", err)
			}
			if err := json.Unmarshal([]byte(testCase.want), &want); err != nil {
				t.Fatal("解码预期快照条目失败", err)
			}
			if !reflect.DeepEqual(actual, want) {
				t.Fatalf("实际 %s，预期 %s", encoded, testCase.want)
			}
		})
	}
}

func TestNewSyncSnapshotEntryRejectsInvalidMetadata(t *testing.T) {
	// 1. 每次只改变一个字段，验证转换拒绝无法公开编码的记录。
	digest := sha256.Sum256([]byte("abc"))
	for _, testCase := range []struct {
		name   string
		change func(*domain.SyncSnapshotEntry)
	}{
		{"zero ID", func(entry *domain.SyncSnapshotEntry) { entry.ID = 0 }},
		{"zero parent", func(entry *domain.SyncSnapshotEntry) { entry.ParentID = 0 }},
		{"self parent", func(entry *domain.SyncSnapshotEntry) { entry.ParentID = entry.ID }},
		{"zero revision", func(entry *domain.SyncSnapshotEntry) { entry.Revision = 0 }},
		{"empty name", func(entry *domain.SyncSnapshotEntry) { entry.Name = "" }},
		{"invalid UTF-8 name", func(entry *domain.SyncSnapshotEntry) { entry.Name = "\xff" }},
		{"unknown type", func(entry *domain.SyncSnapshotEntry) { entry.EntryType = "LINK" }},
		{"unsafe size", func(entry *domain.SyncSnapshotEntry) { entry.FileSize = maxJSONSafeInteger + 1 }},
		{"file missing checksum", func(entry *domain.SyncSnapshotEntry) { entry.VerifiedSHA256 = nil }},
		{"directory with bytes", func(entry *domain.SyncSnapshotEntry) {
			entry.EntryType, entry.VerifiedSHA256 = "DIR", nil
		}},
		{"directory with checksum", func(entry *domain.SyncSnapshotEntry) {
			entry.EntryType, entry.FileSize = "DIR", 0
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			entry := domain.SyncSnapshotEntry{ID: 12, ParentID: 11, Name: "file.txt", EntryType: "FILE", Revision: 1, FileSize: 3, VerifiedSHA256: &digest}
			testCase.change(&entry)
			if data, err := NewSyncSnapshotEntry(entry); data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatalf("异常快照条目应返回目录项数据错误，数据=%+v，错误=%v", data, err)
			}
		})
	}
}
