package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestBuildRenameEntryRequestHashVectors(t *testing.T) {
	for _, testCase := range []struct {
		name, rawName, encoded, wantHex string
		entryID, revision               uint64
	}{
		{name: "trimmed name", entryID: 11, revision: 3, rawName: " \u3000学习资料\u00a0 ",
			encoded: `{"operation":"v1.entries.rename","entry_id":"11","expected_revision":"3","name":"学习资料"}`,
			wantHex: "92ecb0765a4bf93d9f635469d335a3b22ddd42086390d3352f621abcf7486f29"},
		{name: "maximum integers", entryID: ^uint64(0), revision: ^uint64(0), rawName: "目录",
			encoded: `{"operation":"v1.entries.rename","entry_id":"18446744073709551615","expected_revision":"18446744073709551615","name":"目录"}`,
			wantHex: "db031ef31f788933db6d844d8a194fede76cb8738246a7718f12542e23b2da95"},
		{name: "html escaping", entryID: 11, revision: 3, rawName: "A&B + '引用'",
			encoded: `{"operation":"v1.entries.rename","entry_id":"11","expected_revision":"3","name":"A\u0026B + '引用'"}`,
			wantHex: "d2419be6f43f84ab9c60807f3c6abaa0d411fcafa926910816110f30b71316b8"},
		{name: "unicode separators", entryID: 11, revision: 3, rawName: "a\u2028b\u2029c",
			encoded: `{"operation":"v1.entries.rename","entry_id":"11","expected_revision":"3","name":"a\u2028b\u2029c"}`,
			wantHex: "63c26955cbb7c632224f6134ace424654ef73bdd2dc0d5a030473857e4c10061"},
		{name: "decomposed display", entryID: 11, revision: 3, rawName: "Cafe\u0301",
			encoded: "{\"operation\":\"v1.entries.rename\",\"entry_id\":\"11\",\"expected_revision\":\"3\",\"name\":\"Cafe\u0301\"}",
			wantHex: "f06313ea63e7d05990ea7ad62c0e55a00cf7a3062238732a15985ee2ad44b290"},
		{name: "composed display", entryID: 11, revision: 3, rawName: "Café",
			encoded: `{"operation":"v1.entries.rename","entry_id":"11","expected_revision":"3","name":"Café"}`,
			wantHex: "b851befa0865b8e0ee5c9671b994ce91183aa96c472aae0bc42ed73dc7a3e0cc"},
		{name: "high target", entryID: 9007199254740993, revision: 3, rawName: "资料",
			encoded: `{"operation":"v1.entries.rename","entry_id":"9007199254740993","expected_revision":"3","name":"资料"}`,
			wantHex: "393f71ff07d25f3c8d146a4befdaf557cf76df4af05407d72d6efd104bf47f46"},
		{name: "high revision", entryID: 11, revision: 9007199254740993, rawName: "资料",
			encoded: `{"operation":"v1.entries.rename","entry_id":"11","expected_revision":"9007199254740993","name":"资料"}`,
			wantHex: "dea5f72938ffdc812dc8e641468c059968ff4ffe88d7e305a6174e16c194b395"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 用独立 Python 编码和 SHA-256 结果核对固定向量。
			hash, err := BuildRenameEntryRequestHash(testCase.entryID, testCase.revision, testCase.rawName)
			if err != nil || hex.EncodeToString(hash[:]) != testCase.wantHex || hash != sha256.Sum256([]byte(testCase.encoded)) {
				t.Fatal("重命名固定编码或摘要错误", err)
			}
			// 2. 核对重复调用及返回数组的独立性。
			second, err := BuildRenameEntryRequestHash(testCase.entryID, testCase.revision, testCase.rawName)
			if err != nil || second != hash {
				t.Fatal("重复生成摘要不稳定", err)
			}
			hash[0] ^= 0xff
			if hex.EncodeToString(second[:]) != testCase.wantHex {
				t.Fatal("返回摘要未独立持有")
			}
		})
	}
}

func TestBuildRenameEntryRequestHashDifferences(t *testing.T) {
	for _, testCase := range []struct {
		name                      string
		firstID, nextID           uint64
		firstVersion, nextVersion uint64
		firstName, nextName       string
	}{
		{"target", 11, 12, 3, 3, "资料", "资料"},
		{"revision", 11, 11, 3, 4, "资料", "资料"},
		{"name", 11, 11, 3, 3, "资料", "新资料"},
		{"display case", 11, 11, 3, 3, "Notes", "NOTES"},
		{"display spelling", 11, 11, 3, 3, "Café", "Cafe\u0301"},
		{"fold equivalent", 11, 11, 3, 3, "Straße", "STRASSE"},
		{"high target precision", 9007199254740992, 9007199254740993, 3, 3, "资料", "资料"},
		{"high revision precision", 11, 11, 9007199254740992, 9007199254740993, "资料", "资料"},
		{"unsigned target precision", ^uint64(0) - 1, ^uint64(0), 3, 3, "资料", "资料"},
		{"unsigned revision precision", 11, 11, ^uint64(0) - 1, ^uint64(0), "资料", "资料"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			first, firstError := BuildRenameEntryRequestHash(testCase.firstID, testCase.firstVersion, testCase.firstName)
			next, nextError := BuildRenameEntryRequestHash(testCase.nextID, testCase.nextVersion, testCase.nextName)
			if firstError != nil || nextError != nil || first == next {
				t.Fatal("业务参数变化未产生不同摘要", firstError, nextError)
			}
		})
	}
	if domain.RenameEntryOperation != "v1.entries.rename" || len(domain.RenameEntryOperation) > 64 || domain.RenameEntryOperation == domain.CreateDirectoryOperation {
		t.Fatal("版本化操作名错误")
	}
	// 创建和重命名分别使用自己的固定编码与操作名。
	create, createError := BuildCreateDirectoryRequestHash(11, "资料", nil)
	rename, renameError := BuildRenameEntryRequestHash(11, 3, "资料")
	if createError != nil || renameError != nil || create == rename {
		t.Fatal("不同操作的摘要错误", createError, renameError)
	}
}

func TestBuildRenameEntryRequestHashFailures(t *testing.T) {
	for _, testCase := range []struct {
		name, rawName     string
		entryID, revision uint64
		want              error
	}{
		{name: "zero target", revision: 3, rawName: "资料"},
		{name: "zero revision", entryID: 11, rawName: "资料"},
		{name: "both zero before name", rawName: "private/name"},
		{name: "empty name", entryID: 11, revision: 3, want: domain.ErrInvalidEntryName},
		{name: "blank name", entryID: 11, revision: 3, rawName: " \u3000 ", want: domain.ErrInvalidEntryName},
		{name: "path separator", entryID: 11, revision: 3, rawName: "private/name", want: domain.ErrInvalidEntryName},
		{name: "reserved device", entryID: 11, revision: 3, rawName: "CON.txt", want: domain.ErrInvalidEntryName},
		{name: "control before trim", entryID: 11, revision: 3, rawName: "private-name\n", want: domain.ErrInvalidEntryName},
		{name: "invalid utf8", entryID: 11, revision: 3, rawName: "private-name\xff", want: domain.ErrInvalidEntryName},
		{name: "utf16 overflow", entryID: 11, revision: 3, rawName: strings.Repeat("🌱", 61), want: domain.ErrInvalidEntryName},
		{name: "trailing period", entryID: 11, revision: 3, rawName: "private-name.", want: domain.ErrInvalidEntryName},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			hash, err := BuildRenameEntryRequestHash(testCase.entryID, testCase.revision, testCase.rawName)
			if err == nil || hash != [sha256.Size]byte{} || (testCase.want != nil && !errors.Is(err, testCase.want)) || strings.Contains(err.Error(), "private-") || strings.Contains(err.Error(), "private/") {
				t.Fatal("非法输入未返回零值摘要及固定错误", err)
			}
			if testCase.entryID == 0 && testCase.revision == 0 && errors.Is(err, domain.ErrInvalidEntryName) {
				t.Fatal("输入检查顺序错误")
			}
		})
	}
}

func TestBuildRenameEntryRequestHashBoundsAndConcurrent(t *testing.T) {
	for _, name := range []string{strings.Repeat("A", 120), strings.Repeat("中", 120), strings.Repeat("🌱", 60), strings.Repeat("\u0f73", 120)} {
		plain, plainError := BuildRenameEntryRequestHash(1, ^uint64(0), name)
		padded, paddedError := BuildRenameEntryRequestHash(1, ^uint64(0), "\u3000"+name+"\u00a0")
		if plainError != nil || paddedError != nil || plain != padded || plain == [sha256.Size]byte{} {
			t.Fatal("有效名称边界或空白处理错误", plainError, paddedError)
		}
	}
	var workers sync.WaitGroup
	for worker := 0; worker < 64; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for attempt := 0; attempt < 20; attempt++ {
				hash, err := BuildRenameEntryRequestHash(11, 3, " 学习资料 ")
				if err != nil || hex.EncodeToString(hash[:]) != "92ecb0765a4bf93d9f635469d335a3b22ddd42086390d3352f621abcf7486f29" {
					t.Error("并发生成摘要不一致", err)
				}
			}
		}()
	}
	workers.Wait()
}
