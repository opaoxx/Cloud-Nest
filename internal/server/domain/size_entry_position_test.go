package domain

import (
	"fmt"
	"testing"
)

func TestSizeEntryPositionIsValid(t *testing.T) {
	// 1. 组合零字节、JSON/有符号边界及完整无符号范围。
	sizes := []uint64{0, 1, 1024, 1<<53 - 1, 1 << 53, 1<<63 - 1, 1 << 63, ^uint64(0) - 1, ^uint64(0)}
	identifiers := []uint64{0, 1, 19, 1 << 63, ^uint64(0)}
	for _, fileSize := range sizes {
		for _, entryID := range identifiers {
			t.Run(fmt.Sprintf("size=%d/id=%d", fileSize, entryID), func(t *testing.T) {
				position := SizeEntryPosition{FileSize: fileSize, ID: entryID}
				original := position
				// 2. 校验只读取位置值，所有大小使用同一非零标识规则。
				if valid := position.IsValid(); valid != (entryID != 0) {
					t.Fatal("大小位置校验结果错误", valid)
				}
				if position != original {
					t.Fatal("大小位置校验修改了输入")
				}
			})
		}
	}
	if (SizeEntryPosition{}).IsValid() {
		t.Fatal("零值被接受为大小分页位置")
	}
}

func TestSizeEntryPositionFromFileEntry(t *testing.T) {
	for _, fixture := range []struct {
		name  string
		entry FileEntry
	}{
		{name: "empty file", entry: FileEntry{ID: 19, EntryType: "FILE"}},
		{name: "directory", entry: FileEntry{ID: 23, EntryType: "DIR"}},
		{name: "ordinary file", entry: FileEntry{ID: 25, EntryType: "FILE", FileSize: 2048}},
		{name: "maximum unsigned values", entry: FileEntry{ID: ^uint64(0), EntryType: "FILE", FileSize: ^uint64(0)}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 直接保存基础记录的大小及完整标识，形成独立值。
			position := SizeEntryPosition{FileSize: fixture.entry.FileSize, ID: fixture.entry.ID}
			original := position
			if !position.IsValid() || position.FileSize != fixture.entry.FileSize || position.ID != fixture.entry.ID {
				t.Fatal("基础记录到大小位置的转换错误")
			}
			// 2. 分别修改来源记录和位置副本，核对原位置的值。
			fixture.entry.FileSize, fixture.entry.ID = 0, 0
			copied := position
			copied.FileSize, copied.ID = 0, 0
			if position != original || copied.IsValid() {
				t.Fatal("大小位置与来源或副本互相影响")
			}
		})
	}
}
