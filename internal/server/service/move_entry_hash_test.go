package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestBuildMoveEntryRequestHashVectors(t *testing.T) {
	for _, testCase := range []struct {
		name, encoded, wantHex            string
		entryID, revision, targetParentID uint64
	}{
		{name: "minimum", entryID: 1, revision: 1, targetParentID: 1,
			encoded: `{"operation":"v1.entries.move","entry_id":"1","expected_revision":"1","target_parent_id":"1"}`,
			wantHex: "546ac5aa9b8bc14845f1a8e9494fddaa105311f00b429a4a07e7438a1d550fcc"},
		{name: "ordinary", entryID: 13, revision: 3, targetParentID: 14,
			encoded: `{"operation":"v1.entries.move","entry_id":"13","expected_revision":"3","target_parent_id":"14"}`,
			wantHex: "749e2fe46f05dddb8cf202ad29660149f44458f026042fca19bb4d68e2525172"},
		{name: "high source", entryID: 9007199254740993, revision: 3, targetParentID: 14,
			encoded: `{"operation":"v1.entries.move","entry_id":"9007199254740993","expected_revision":"3","target_parent_id":"14"}`,
			wantHex: "ce9b2f1c9e9590d7cd030e39a2dde6f913ad21b4dace69523e3d319aaaed4b60"},
		{name: "high revision", entryID: 13, revision: 9007199254740993, targetParentID: 14,
			encoded: `{"operation":"v1.entries.move","entry_id":"13","expected_revision":"9007199254740993","target_parent_id":"14"}`,
			wantHex: "e2b1783164485b695b9896fc561ab2ca430bc910f35a22cf1969c6ead1435454"},
		{name: "high destination", entryID: 13, revision: 3, targetParentID: 9007199254740993,
			encoded: `{"operation":"v1.entries.move","entry_id":"13","expected_revision":"3","target_parent_id":"9007199254740993"}`,
			wantHex: "698ac57ef6263a7e3ff7075611c52f4a434e43c10a110d03263673970e0ec7d5"},
		{name: "maximum", entryID: ^uint64(0), revision: ^uint64(0), targetParentID: ^uint64(0),
			encoded: `{"operation":"v1.entries.move","entry_id":"18446744073709551615","expected_revision":"18446744073709551615","target_parent_id":"18446744073709551615"}`,
			wantHex: "8a43c7d706233e689bcb4870cd33ae3cd1f016753d2b6e9425bc16323baf9b93"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 用独立 Python 编码和 SHA-256 结果核对固定向量。
			hash, err := BuildMoveEntryRequestHash(testCase.entryID, testCase.revision, testCase.targetParentID)
			if err != nil || hex.EncodeToString(hash[:]) != testCase.wantHex || hash != sha256.Sum256([]byte(testCase.encoded)) {
				t.Fatal("移动固定编码或摘要错误", err)
			}
			// 2. 核对重复调用以及返回数组的独立性。
			second, err := BuildMoveEntryRequestHash(testCase.entryID, testCase.revision, testCase.targetParentID)
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

func TestBuildMoveEntryRequestHashDifferences(t *testing.T) {
	for _, testCase := range []struct {
		name                                  string
		firstID, firstRevision, firstParentID uint64
		nextID, nextRevision, nextParentID    uint64
	}{
		{"source", 13, 3, 14, 15, 3, 14},
		{"revision", 13, 3, 14, 13, 4, 14},
		{"destination", 13, 3, 14, 13, 3, 15},
		{"high source precision", 9007199254740992, 3, 14, 9007199254740993, 3, 14},
		{"high revision precision", 13, 9007199254740992, 14, 13, 9007199254740993, 14},
		{"high destination precision", 13, 3, 9007199254740992, 13, 3, 9007199254740993},
		{"maximum source precision", ^uint64(0) - 1, 3, 14, ^uint64(0), 3, 14},
		{"maximum revision precision", 13, ^uint64(0) - 1, 14, 13, ^uint64(0), 14},
		{"maximum destination precision", 13, 3, ^uint64(0) - 1, 13, 3, ^uint64(0)},
		{"source destination exchange", 13, 3, 14, 14, 3, 13},
		{"field boundaries", 1, 23, 4, 12, 3, 4},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			first, firstError := BuildMoveEntryRequestHash(testCase.firstID, testCase.firstRevision, testCase.firstParentID)
			next, nextError := BuildMoveEntryRequestHash(testCase.nextID, testCase.nextRevision, testCase.nextParentID)
			if firstError != nil || nextError != nil || first == next {
				t.Fatal("业务参数变化未产生不同摘要", firstError, nextError)
			}
		})
	}
	if domain.MoveEntryOperation != "v1.entries.move" || len(domain.MoveEntryOperation) > 64 || domain.MoveEntryOperation == domain.CreateDirectoryOperation || domain.MoveEntryOperation == domain.RenameEntryOperation {
		t.Fatal("版本化操作名错误")
	}
	move, moveError := BuildMoveEntryRequestHash(13, 3, 14)
	rename, renameError := BuildRenameEntryRequestHash(13, 3, "14")
	create, createError := BuildCreateDirectoryRequestHash(14, "13", nil)
	if moveError != nil || renameError != nil || createError != nil || move == rename || move == create {
		t.Fatal("不同操作的摘要错误", moveError, renameError, createError)
	}
}

func TestBuildMoveEntryRequestHashFailures(t *testing.T) {
	for _, input := range [][3]uint64{{0, 3, 14}, {13, 0, 14}, {13, 3, 0}, {0, 0, 14}, {0, 3, 0}, {13, 0, 0}, {0, 0, 0}} {
		t.Run(fmt.Sprint(input), func(t *testing.T) {
			hash, err := BuildMoveEntryRequestHash(input[0], input[1], input[2])
			if err == nil || hash != [sha256.Size]byte{} || err.Error() != "生成移动请求摘要需要非零源 ID、预期版本和目标父目录 ID" {
				t.Fatal("非法输入未返回零值摘要及固定错误", err)
			}
		})
	}
}

func TestBuildMoveEntryRequestHashBoundsAndConcurrent(t *testing.T) {
	// 1. 组合检查最小值、安全整数边界及无符号整数上限。
	identifiers := []uint64{1, 9007199254740992, 9007199254740993, 1 << 63, ^uint64(0)}
	for _, entryID := range identifiers {
		for _, revision := range identifiers {
			for _, targetParentID := range identifiers {
				encoded := fmt.Sprintf(`{"operation":"v1.entries.move","entry_id":"%d","expected_revision":"%d","target_parent_id":"%d"}`, entryID, revision, targetParentID)
				hash, err := BuildMoveEntryRequestHash(entryID, revision, targetParentID)
				if err != nil || hash != sha256.Sum256([]byte(encoded)) {
					t.Fatal("完整整数编码错误", err)
				}
			}
		}
	}
	// 2. 并发调用使用相同固定向量，检查稳定输出。
	var workers sync.WaitGroup
	for worker := 0; worker < 64; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for attempt := 0; attempt < 20; attempt++ {
				hash, err := BuildMoveEntryRequestHash(13, 3, 14)
				if err != nil || hex.EncodeToString(hash[:]) != "749e2fe46f05dddb8cf202ad29660149f44458f026042fca19bb4d68e2525172" {
					t.Error("并发生成摘要不一致", err)
				}
			}
		}()
	}
	workers.Wait()
}
