package domain

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestNameEntryPositionIsValid(t *testing.T) {
	// 1. 准备标识、数据库字节容量以及原始二进制键的边界。
	for _, testCase := range []struct {
		name     string
		position NameEntryPosition
		valid    bool
	}{
		{name: "zero value"},
		{name: "zero ID", position: NameEntryPosition{NameKey: []byte("notes.md")}},
		{name: "nil key", position: NameEntryPosition{ID: 19}},
		{name: "empty key", position: NameEntryPosition{NameKey: []byte{}, ID: 19}},
		{name: "minimum ID", position: NameEntryPosition{NameKey: []byte("a"), ID: 1}, valid: true},
		{name: "maximum unsigned ID", position: NameEntryPosition{NameKey: []byte("notes.md"), ID: ^uint64(0)}, valid: true},
		{name: "high unsigned ID", position: NameEntryPosition{NameKey: []byte("notes.md"), ID: 1 << 63}, valid: true},
		{name: "ASCII key", position: NameEntryPosition{NameKey: []byte("notes.md"), ID: 19}, valid: true},
		{name: "Chinese key", position: NameEntryPosition{NameKey: []byte("学习笔记.md"), ID: 19}, valid: true},
		{name: "emoji key", position: NameEntryPosition{NameKey: []byte("笔记🌱.md"), ID: 19}, valid: true},
		{name: "folded expansion", position: NameEntryPosition{NameKey: []byte("strasse.txt"), ID: 19}, valid: true},
		{name: "composed bytes", position: NameEntryPosition{NameKey: []byte("café.md"), ID: 19}, valid: true},
		{name: "decomposed stored bytes", position: NameEntryPosition{NameKey: []byte("cafe\u0301.md"), ID: 19}, valid: true},
		{name: "uppercase stored bytes", position: NameEntryPosition{NameKey: []byte("Notes.MD"), ID: 19}, valid: true},
		{name: "stored spaces", position: NameEntryPosition{NameKey: []byte(" notes.md "), ID: 19}, valid: true},
		{name: "one zero byte", position: NameEntryPosition{NameKey: []byte{0}, ID: 19}, valid: true},
		{name: "one non UTF-8 byte", position: NameEntryPosition{NameKey: []byte{0xff}, ID: 19}, valid: true},
		{name: "mixed binary bytes", position: NameEntryPosition{NameKey: []byte{0, 0xff, 0x80, 'a'}, ID: 19}, valid: true},
		{name: "767 bytes", position: NameEntryPosition{NameKey: bytes.Repeat([]byte{'a'}, 767), ID: 19}, valid: true},
		{name: "768 bytes", position: NameEntryPosition{NameKey: bytes.Repeat([]byte{'a'}, 768), ID: 19}, valid: true},
		{name: "769 bytes", position: NameEntryPosition{NameKey: bytes.Repeat([]byte{'a'}, 769), ID: 19}},
		{name: "768 Chinese bytes", position: NameEntryPosition{NameKey: []byte(strings.Repeat("界", 256)), ID: 19}, valid: true},
		{name: "771 Chinese bytes", position: NameEntryPosition{NameKey: []byte(strings.Repeat("界", 257)), ID: 19}},
		{name: "768 emoji bytes", position: NameEntryPosition{NameKey: []byte(strings.Repeat("🌱", 192)), ID: 19}, valid: true},
		{name: "772 emoji bytes", position: NameEntryPosition{NameKey: []byte(strings.Repeat("🌱", 193)), ID: 19}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 2. 校验位置，核对原始键字节、空值形态及完整 ID。
			originalKey := bytes.Clone(testCase.position.NameKey)
			originalID := testCase.position.ID
			if valid := testCase.position.IsValid(); valid != testCase.valid {
				t.Fatalf("名称位置校验结果为 %v，期望 %v", valid, testCase.valid)
			}
			if testCase.position.ID != originalID || !reflect.DeepEqual(testCase.position.NameKey, originalKey) {
				t.Fatal("名称位置校验修改了输入")
			}
		})
	}
}

func TestNameEntryPositionPreservesBorrowedBytes(t *testing.T) {
	// 1. 使用带额外容量的切片视图，保留视图外的哨兵字节。
	storage := append([]byte{0xaa}, []byte(" Notes.MD ")...)
	storage = append(storage, 0xbb)
	position := NameEntryPosition{NameKey: storage[1 : len(storage)-1], ID: ^uint64(0)}
	original := bytes.Clone(storage)
	originalLength, originalCapacity := len(position.NameKey), cap(position.NameKey)

	// 2. 重复校验共享字节视图，检查内容、长度和容量。
	for range 3 {
		if !position.IsValid() || !bytes.Equal(storage, original) || len(position.NameKey) != originalLength || cap(position.NameKey) != originalCapacity {
			t.Fatal("校验改变了借用的名称键或底层存储")
		}
	}
}
