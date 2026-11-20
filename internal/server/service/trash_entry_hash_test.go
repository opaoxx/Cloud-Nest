package service

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestBuildTrashEntryRequestHashVectors(t *testing.T) {
	// 1. 用独立的 Python JSON 编码和 SHA-256 结果核对固定向量。
	for _, fixture := range []struct {
		name, wantHex     string
		entryID, revision uint64
	}{
		{"minimum", "95f88e1ff75594f3acebb572dc6cba573858fb60e800760d11ba180bbed1e107", 1, 1},
		{"ordinary", "0d3697e1eff96f652623882e031035bf748f53be6703a210ea4d105ecda7ffc6", 23, 3},
		{"high ID", "b65aec9d5ac66ba9ee848b47926ba24a6c7a286a820117c0415c7b8bf830f5f8", 9007199254740993, 3},
		{"high revision", "a44a910aff0740c2a432d277ba39ab872f510b42b188ae0c88b6b416d2790e23", 23, 9007199254740993},
		{"maximum", "c2944363965747d031d6502f74284fe3f3cd38a3d78507d628494dd575705548", math.MaxUint64, math.MaxUint64},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			first, err := BuildTrashEntryRequestHash(fixture.entryID, fixture.revision)
			if err != nil || hex.EncodeToString(first[:]) != fixture.wantHex {
				t.Fatal("回收摘要与固定向量不符", err)
			}

			// 2. 重算同一组参数并核对结果稳定。
			repeated, err := BuildTrashEntryRequestHash(fixture.entryID, fixture.revision)
			if err != nil || repeated != first {
				t.Fatal("相同参数重新生成的回收摘要不符", err)
			}
		})
	}
}

func TestBuildTrashEntryRequestHashDifferences(t *testing.T) {
	// 1. 单独改变目标或版本，包含高位相邻值和字段边界。
	for _, fixture := range []struct {
		name                                          string
		entryID, revision, changedID, changedRevision uint64
	}{
		{"target", 23, 3, 24, 3},
		{"revision", 23, 3, 23, 4},
		{"adjacent large IDs", 9007199254740992, 3, 9007199254740993, 3},
		{"adjacent maximum revisions", 23, math.MaxUint64 - 1, 23, math.MaxUint64},
		{"field boundaries", 1, 23, 12, 3},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			original, originalError := BuildTrashEntryRequestHash(fixture.entryID, fixture.revision)
			changed, changedError := BuildTrashEntryRequestHash(fixture.changedID, fixture.changedRevision)
			if originalError != nil || changedError != nil || original == changed {
				t.Fatal("改变回收参数未产生不同摘要", originalError, changedError)
			}
		})
	}

	// 2. 核对操作名命名空间及相同目标版本的不同操作。
	trash, trashError := BuildTrashEntryRequestHash(23, 3)
	move, moveError := BuildMoveEntryRequestHash(23, 3, 12)
	rename, renameError := BuildRenameEntryRequestHash(23, 3, "笔记.md")
	if domain.TrashEntryOperation == domain.MoveEntryOperation || domain.TrashEntryOperation == domain.RenameEntryOperation ||
		trashError != nil || moveError != nil || renameError != nil || trash == move || trash == rename {
		t.Fatal("回收、移动和重命名的摘要未区分", trashError, moveError, renameError)
	}
}

func TestBuildTrashEntryRequestHashRejectsZero(t *testing.T) {
	for _, fixture := range []struct {
		name              string
		entryID, revision uint64
	}{
		{"zero target", 0, 3},
		{"zero revision", 23, 0},
		{"both zero", 0, 0},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			hash, err := BuildTrashEntryRequestHash(fixture.entryID, fixture.revision)
			if hash != [sha256.Size]byte{} || err == nil || err.Error() != "生成回收请求摘要需要非零目标 ID 和预期版本" {
				t.Fatal("非法回收参数未返回固定错误和零值摘要", err)
			}
		})
	}
}
