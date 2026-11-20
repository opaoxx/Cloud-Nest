package service

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"testing"
)

func TestBuildRestoreEntryRequestHashVectors(t *testing.T) {
	// 1. 用独立生成的固定向量核对编码和完整整数精度。
	for _, testCase := range []struct {
		name, wantHex     string
		entryID, revision uint64
	}{
		{"minimum", "2dd62740829f6e214406d9027a21a60232d2db0a14271148ef972dd2c01c0bcc", 1, 1},
		{"standard", "d951119fa88918b80c25020bda1d682add36ba5732365931213f9e05c02a602c", 23, 4},
		{"large ID", "e34cc732577d4a97808fe09dd72dee1398b11699c6ba6becd5c367adc536af4c", 9007199254740993, 4},
		{"large revision", "801083bf857af1e753a29b750ac09c6ad3e8f0ffa9f81e31d9aebf18aff7e6cc", 23, 9007199254740993},
		{"maximum values", "67599aeb66f9cde9997a13d826efd93e64a0b64fbd0beaa2546f1b32847acb23", math.MaxUint64, math.MaxUint64},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			digest, err := BuildRestoreEntryRequestHash(testCase.entryID, testCase.revision)
			if err != nil || hex.EncodeToString(digest[:]) != testCase.wantHex {
				t.Fatalf("摘要为 %x，期望 %s，错误为 %v", digest, testCase.wantHex, err)
			}
			// 2. 用同一组输入重新计算，确认摘要可用于重复请求比较。
			repeated, err := BuildRestoreEntryRequestHash(testCase.entryID, testCase.revision)
			if err != nil || repeated != digest {
				t.Fatalf("重试摘要为 %x，错误为 %v", repeated, err)
			}
		})
	}
}

func TestBuildRestoreEntryRequestHashDistinguishesOperations(t *testing.T) {
	// 1. 同一目标及版本在恢复和回收操作中使用不同摘要。
	restore, restoreError := BuildRestoreEntryRequestHash(23, 4)
	trash, trashError := BuildTrashEntryRequestHash(23, 4)
	if restoreError != nil || trashError != nil || restore == trash {
		t.Fatalf("恢复和回收摘要为 %x、%x，错误为 %v、%v", restore, trash, restoreError, trashError)
	}
}

func TestBuildRestoreEntryRequestHashRejectsZero(t *testing.T) {
	for _, testCase := range []struct {
		name              string
		entryID, revision uint64
	}{
		{"zero target", 0, 4},
		{"zero revision", 23, 0},
		{"both zero", 0, 0},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			digest, err := BuildRestoreEntryRequestHash(testCase.entryID, testCase.revision)
			if digest != [sha256.Size]byte{} || err == nil {
				t.Fatalf("无效参数返回了 %x，错误为 %v", digest, err)
			}
		})
	}
}
