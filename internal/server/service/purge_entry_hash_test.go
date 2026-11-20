package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const purgeHashExampleDigest = "eyJ2IjoxfQ.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestBuildPurgeEntryRequestHashVectors(t *testing.T) {
	maximumDigest := strings.Repeat("A", domain.MaxPurgePreviewDigestLength-len(purgeHashExampleDigest)) + purgeHashExampleDigest
	// 1. 核对使用 Python 独立生成的固定向量和完整无符号范围。
	for _, scenario := range []struct {
		name, previewDigest, expectedHex string
		entryID, revision                uint64
	}{
		{"standard", purgeHashExampleDigest, "d3ec47bbacdf76f17890759f1c14014e1ea7ff50b15aa88c36678f722d014647", 23, 4},
		{"minimum", purgeHashExampleDigest, "31761469309868365c7383074d1f97308548c5dba9559c1e9e169a49e4a06d8b", 1, 1},
		{"maximum", maximumDigest, "b89f10de685865486891ae8b431c9e0bc4c7c39d32283c290f7a7771f3376a3b", math.MaxUint64, math.MaxUint64},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			for range 2 {
				digest, err := BuildPurgeEntryRequestHash(scenario.entryID, scenario.revision, scenario.previewDigest)
				if err != nil || hex.EncodeToString(digest[:]) != scenario.expectedHex {
					t.Fatalf("摘要为 %x，期望 %s，错误 %v", digest, scenario.expectedHex, err)
				}
			}
		})
	}
}

func TestBuildPurgeEntryRequestHashBindsEntireCredential(t *testing.T) {
	// 1. 为同一子树在两个不同的时刻签发凭证。
	signer, err := NewPurgePreviewSigner(make([]byte, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := sha256.Sum256([]byte("same fixture subtree"))
	issuedAt := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	first, err := signer.Sign(7, 23, fingerprint, issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	second, err := signer.Sign(7, 23, fingerprint, issuedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	// 2. 历史凭证仍可用于重算原请求，重签、改目标或改版本均改变摘要。
	original, err := BuildPurgeEntryRequestHash(23, 4, first.PreviewDigest)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name, previewDigest string
		entryID, revision   uint64
		wantSame            bool
	}{
		{"identical retry", first.PreviewDigest, 23, 4, true},
		{"new credential for the same subtree", second.PreviewDigest, 23, 4, false},
		{"different entry", first.PreviewDigest, 24, 4, false},
		{"different revision", first.PreviewDigest, 23, 5, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			digest, err := BuildPurgeEntryRequestHash(scenario.entryID, scenario.revision, scenario.previewDigest)
			if err != nil || (digest == original) != scenario.wantSame {
				t.Fatal("确认参数与请求摘要的对应关系错误", err)
			}
		})
	}
}

func TestBuildPurgeEntryRequestHashRejectsInvalidInput(t *testing.T) {
	for _, scenario := range []struct {
		name, previewDigest string
		entryID, revision   uint64
		wantError           error
	}{
		{"zero entry ID", purgeHashExampleDigest, 0, 4, nil},
		{"zero revision", purgeHashExampleDigest, 23, 0, nil},
		{"empty credential", "", 23, 4, domain.ErrInvalidPurgePreviewDigest},
		{"wrong separator", strings.Replace(purgeHashExampleDigest, ".", ":", 1), 23, 4, domain.ErrInvalidPurgePreviewDigest},
		{"padded credential", purgeHashExampleDigest + "=", 23, 4, domain.ErrInvalidPurgePreviewDigest},
		{"too long", strings.Repeat("A", domain.MaxPurgePreviewDigestLength+1-len(purgeHashExampleDigest)) + purgeHashExampleDigest, 23, 4, domain.ErrInvalidPurgePreviewDigest},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			digest, err := BuildPurgeEntryRequestHash(scenario.entryID, scenario.revision, scenario.previewDigest)
			if digest != ([sha256.Size]byte{}) || err == nil || (scenario.wantError != nil && !errors.Is(err, scenario.wantError)) {
				t.Fatal("非法输入应返回零值摘要和固定错误", err)
			}
		})
	}
}
