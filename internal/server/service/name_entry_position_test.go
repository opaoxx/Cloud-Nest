package service

import (
	"bytes"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestNameEntryPositionWithGeneratedKeys(t *testing.T) {
	// 1. 使用正式名称键生成入口，覆盖大小写、Unicode 组合与折叠。
	for _, testCase := range []struct {
		name, displayName, equivalentName, expectedKey string
	}{
		{name: "ASCII case", displayName: " Notes.MD ", equivalentName: "notes.md", expectedKey: "notes.md"},
		{name: "Unicode composition", displayName: "Cafe\u0301.MD", equivalentName: "Café.md", expectedKey: "café.md"},
		{name: "case folding expansion", displayName: "STRAẞE.txt", equivalentName: "strasse.txt", expectedKey: "strasse.txt"},
		{name: "Chinese and emoji", displayName: "学习笔记🌱.MD", equivalentName: "学习笔记🌱.md", expectedKey: "学习笔记🌱.md"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			nameKey, err := BuildEntryNameKey(testCase.displayName)
			if err != nil {
				t.Fatal(err)
			}
			equivalentKey, err := BuildEntryNameKey(testCase.equivalentName)
			if err != nil || !bytes.Equal(nameKey, equivalentKey) || string(nameKey) != testCase.expectedKey {
				t.Fatal("名称键生成结果与既有规则不一致", err)
			}

			// 2. 相同键可属于不同目录项，分别以完整 ID 表达位置。
			first := domain.NameEntryPosition{NameKey: nameKey, ID: 19}
			second := domain.NameEntryPosition{NameKey: equivalentKey, ID: ^uint64(0)}
			original := bytes.Clone(nameKey)
			if !first.IsValid() || !second.IsValid() || !bytes.Equal(first.NameKey, second.NameKey) || first.ID >= second.ID || !bytes.Equal(nameKey, original) {
				t.Fatal("生成名称键未衔接名称分页位置")
			}
		})
	}
}
