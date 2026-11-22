package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestBuildFileReuseRequestHashVectors(t *testing.T) {
	const contentHex = "d125f628ab4f65c600f82c8ad68462fd1383c0af64e476c1342dc96ded9e33f5"
	fixtureDigest, err := hex.DecodeString(contentHex)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		label, name, wantHex string
		parentID, sizeBytes  uint64
		digest               [sha256.Size]byte
	}{
		{"trim display", " \u3000笔记.PDF\u00a0 ", "989a8321857e6e2fbabbd9fd0efa514ccf4e1758b316d7f91ad95c24561de77b", 11, 16777217, [sha256.Size]byte(fixtureDigest)},
		{"maximum parent and zero bytes", "空文件", "8188c720c7f42713b93cde77dc638b7935fcf70503f8c43eb75c859c09a2ea6a", math.MaxUint64, 0, sha256.Sum256(nil)},
		{"maximum file size and escaped name", "A&B.bin", "88cefd15687de90b3a25208ebfdf9b254d1e80bb25263e172f5a20d57c082d13", math.MaxUint64, 1<<53 - 1, [sha256.Size]byte(fixtureDigest)},
	} {
		t.Run(scenario.label, func(t *testing.T) {
			// 1. 与 PowerShell 独立计算的固定字节摘要比对。
			got, err := BuildFileReuseRequestHash(scenario.parentID, scenario.name, scenario.sizeBytes, scenario.digest)
			if err != nil || hex.EncodeToString(got[:]) != scenario.wantHex {
				t.Fatalf("请求摘要得到 %x，错误 %v，期望 %s", got, err, scenario.wantHex)
			}
		})
	}
}

func TestBuildFileReuseRequestHashDistinguishesDeclarations(t *testing.T) {
	const name = "Café.PDF"
	const parentID, sizeBytes = 11, 2048
	content := sha256.Sum256([]byte("synthetic content"))
	otherContent := sha256.Sum256([]byte("changed synthetic content"))
	original, err := BuildFileReuseRequestHash(parentID, name, sizeBytes, content)
	if err != nil {
		t.Fatal(err)
	}

	// 1. 名称首尾空白与内容摘要的十六进制大小写，规范化后与原声明相同。
	uppercaseDigest, err := hex.DecodeString(strings.ToUpper(hex.EncodeToString(content[:])))
	if err != nil {
		t.Fatal(err)
	}
	equivalent, err := BuildFileReuseRequestHash(parentID, " \u3000"+name+"\u00a0 ", sizeBytes, [sha256.Size]byte(uppercaseDigest))
	if err != nil || equivalent != original {
		t.Fatal("等价声明未得到同一摘要", err)
	}

	// 2. 修改任何声明字段都应生成不同摘要，展示名保留实际写法。
	for _, scenario := range []struct {
		label, name         string
		parentID, sizeBytes uint64
		digest              [sha256.Size]byte
	}{
		{"directory", name, parentID + 1, sizeBytes, content},
		{"display name", "新笔记.PDF", parentID, sizeBytes, content},
		{"display case", "Café.pdf", parentID, sizeBytes, content},
		{"display unicode spelling", "Cafe\u0301.PDF", parentID, sizeBytes, content},
		{"size", name, parentID, sizeBytes + 1, content},
		{"content", name, parentID, sizeBytes, otherContent},
	} {
		t.Run(scenario.label, func(t *testing.T) {
			next, err := BuildFileReuseRequestHash(scenario.parentID, scenario.name, scenario.sizeBytes, scenario.digest)
			if err != nil || next == original {
				t.Fatal("变更声明未得到不同摘要", err)
			}
		})
	}
}

func TestBuildFileReuseRequestHashRejectsInvalidDeclarations(t *testing.T) {
	reuse := NewFileReuseService(func(context.Context, domain.Actor, func(context.Context, FileReuseTransaction) error) error {
		t.Fatal("无效声明不应进入用户事务")
		return nil
	})
	digest := sha256.Sum256(nil)
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	for _, scenario := range []struct {
		label, name         string
		parentID, sizeBytes uint64
		wantError           error
	}{
		{"zero parent", "空文件", 0, 0, nil},
		{"size exceeds JSON bound", "大文件", 11, 1 << 53, nil},
		{"blank name", " \u3000 ", 11, 0, domain.ErrInvalidEntryName},
		{"path separator", "invalid/file.txt", 11, 0, domain.ErrInvalidEntryName},
		{"reserved device name", "CON.txt", 11, 0, domain.ErrInvalidEntryName},
	} {
		t.Run(scenario.label, func(t *testing.T) {
			// 1. 摘要和实际创建共同拒绝相同声明，摘要失败时返回零值。
			hash, hashError := BuildFileReuseRequestHash(scenario.parentID, scenario.name, scenario.sizeBytes, digest)
			entry, createError := reuse.Reuse(t.Context(), actor, scenario.parentID, scenario.name, scenario.sizeBytes, digest)
			if hash != [sha256.Size]byte{} || entry != nil || hashError == nil || createError == nil || hashError.Error() != createError.Error() ||
				(scenario.wantError != nil && !errors.Is(hashError, scenario.wantError)) {
				t.Fatalf("未一致拒绝声明，摘要 %x，条目 %+v，错误 %v 和 %v", hash, entry, hashError, createError)
			}
		})
	}
}
