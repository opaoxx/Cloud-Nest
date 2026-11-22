package dto

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestEncodeSyncSnapshotEntriesJSON(t *testing.T) {
	// 1. 核对空树的数组类型和基本开销。
	empty, err := EncodeSyncSnapshotEntries(nil)
	if err != nil || empty == nil || empty.Items == nil || len(empty.Items) != 0 || empty.SizeBytes != 2 {
		t.Fatalf("空树应为 2 字节空数组，数据=%+v，错误=%v", empty, err)
	}
	encodedEmpty, err := json.Marshal(empty.Items)
	if err != nil || string(encodedEmpty) != "[]" {
		t.Fatalf("空树 JSON=%s，错误=%v", encodedEmpty, err)
	}

	// 2. 用完整数组的实际编码核对多字节名称、转义及分隔符。
	entries := []domain.SyncSnapshotEntry{
		{ID: 12, ParentID: 11, Name: "资料&🌱", EntryType: "DIR", Revision: 3},
		{ID: 13, ParentID: 12, Name: "子目录", EntryType: "DIR", Revision: 1},
	}
	result, err := EncodeSyncSnapshotEntries(entries)
	if err != nil || result == nil || len(result.Items) != 2 {
		t.Fatalf("两个目录应完整编码，数据=%+v，错误=%v", result, err)
	}
	const expected = `[{"entry_id":"12","parent_id":"11","name":"资料\u0026🌱","entry_type":"DIR","revision":"3","size_bytes":0,"sha256":null},{"entry_id":"13","parent_id":"12","name":"子目录","entry_type":"DIR","revision":"1","size_bytes":0,"sha256":null}]`
	encoded, err := json.Marshal(result.Items)
	if err != nil || string(encoded) != expected || result.SizeBytes != len(encoded) {
		t.Fatalf("数组或预算不符，JSON=%s，统计字节=%d，实际字节=%d，错误=%v", encoded, result.SizeBytes, len(encoded), err)
	}
}

func TestEncodeSyncSnapshotEntriesByteLimit(t *testing.T) {
	// 1. 使用相同数字宽度和有效文件名，填出恰好 8 MiB 的真实 JSON 数组。
	const limit = 8388608
	base := domain.SyncSnapshotEntry{
		ID: math.MaxUint64 - domain.MaxSyncSnapshotEntries, ParentID: math.MaxUint64,
		Name: "a", EntryType: "FILE", Revision: math.MaxUint64, FileSize: maxJSONSafeInteger,
	}
	digest := sha256.Sum256([]byte("fixture"))
	base.VerifiedSHA256 = &digest
	item, err := NewSyncSnapshotEntry(base)
	if err != nil {
		t.Fatal("条目夹具转换失败", err)
	}
	encodedItem, err := json.Marshal(item)
	if err != nil {
		t.Fatal("条目夹具编码失败", err)
	}
	entries := make([]domain.SyncSnapshotEntry, domain.MaxSyncSnapshotEntries)
	arrayOverhead := len("[]") + len(entries) - 1
	remaining := limit - len(encodedItem)*len(entries) - arrayOverhead
	if remaining < 0 {
		t.Fatal("基础条目已经超过测试预算")
	}
	for index := range entries {
		entry := base
		entry.ID += uint64(index)
		// & 编码为 6 字节，a 编码为 1 字节。每个合成名称长度最多 120 字符。
		escapedLength := min(remaining+1, 115*6)
		entry.Name = strings.Repeat("&", escapedLength/6) + strings.Repeat("a", escapedLength%6)
		remaining -= escapedLength - 1
		entries[index] = entry
	}
	if remaining != 0 {
		t.Fatalf("夹具未填满预算，差 %d 字节", remaining)
	}

	// 2. 核对达到上限时完整交付，按实际 JSON 最终长度复核。
	result, err := EncodeSyncSnapshotEntries(entries)
	if err != nil || result == nil || len(result.Items) != len(entries) || result.SizeBytes != limit {
		t.Fatalf("恰好 8 MiB 且 10000 项应完整交付，错误=%v", err)
	}
	encoded, err := json.Marshal(result.Items)
	if err != nil || len(encoded) != limit {
		t.Fatalf("完整 JSON 实际大小=%d，预期=%d，错误=%v", len(encoded), limit, err)
	}

	// 3. 末项增加一个 ASCII 字符，确认超限整份拒绝且不影响已编码结果。
	entries[len(entries)-1].Name += "a"
	if result, err := EncodeSyncSnapshotEntries(entries); result != nil || !errors.Is(err, domain.ErrSyncSnapshotTooLarge) {
		t.Fatalf("8 MiB 加 1 字节应返回大小超限，错误=%v", err)
	}
	if !bytes.HasSuffix(encoded[:len(encoded)-1], result.Items[len(result.Items)-1]) {
		t.Fatal("修改输入影响了已经编码的末项")
	}
}

func TestEncodeSyncSnapshotEntriesRejectsInvalidInput(t *testing.T) {
	// 1. 条目数独立限量，超出 10000 项时直接拒绝。
	tooMany := make([]domain.SyncSnapshotEntry, domain.MaxSyncSnapshotEntries+1)
	if result, err := EncodeSyncSnapshotEntries(tooMany); result != nil || !errors.Is(err, domain.ErrSyncSnapshotTooLarge) {
		t.Fatalf("超出条目上限应拒绝，错误=%v", err)
	}

	// 2. 第二项缺少文件摘要，验证错误时不会交付第一项。
	entries := []domain.SyncSnapshotEntry{
		{ID: 12, ParentID: 11, Name: "目录", EntryType: "DIR", Revision: 1},
		{ID: 13, ParentID: 12, Name: "文件", EntryType: "FILE", Revision: 1},
	}
	if result, err := EncodeSyncSnapshotEntries(entries); result != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
		t.Fatalf("含非法条目的快照应整体拒绝，错误=%v", err)
	}
}
