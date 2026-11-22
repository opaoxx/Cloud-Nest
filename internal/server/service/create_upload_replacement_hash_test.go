package service_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestBuildCreateUploadReplacementRequestHashVectors(t *testing.T) {
	if operation := domain.CreateUploadReplacementOperation; len(operation) > 64 ||
		operation == domain.CreateUploadOperation || operation == domain.FileReuseOperation {
		t.Fatalf("替换操作名必须独立且适合幂等记录列，得到 %q", operation)
	}

	for _, fixture := range []struct {
		name, wantHash               string
		entryID, revision, sizeBytes uint64
		digest                       [sha256.Size]byte
	}{
		{"OpenAPI declaration", "ea53a00816a42361dd7f518c6e05fd28a0c15a12bea7c9e37564298151143375",
			31, 4, 9, sha256.Sum256([]byte("123456789"))},
		{"maximum ID and revision with empty file", "74138449033c77dbaee844ebcff0988973ba11ba755f2761b3f23d0c3d279395",
			math.MaxUint64, math.MaxUint64, 0, sha256.Sum256(nil)},
		{"large integers and explicit zero digest", "7f2f48f245fea04790e779fc71f3dfdf08afda8251baf6b3757ece39386fe6e7",
			9007199254740993, 9007199254740995, 1<<53 - 1, [sha256.Size]byte{}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 与 PowerShell 独立计算的固定摘要比对，保护已保存回执使用的编码。
			actual, err := service.BuildCreateUploadReplacementRequestHash(fixture.entryID, fixture.revision, fixture.sizeBytes, fixture.digest)
			if err != nil || hex.EncodeToString(actual[:]) != fixture.wantHash {
				t.Fatalf("请求摘要得到 %x，错误 %v，期望 %s", actual, err, fixture.wantHash)
			}
		})
	}
}

func TestBuildCreateUploadReplacementRequestHashFromDTO(t *testing.T) {
	// 1. 将写法不同但参数相同的两个正文交给上一步的 DTO。
	contentDigest := sha256.Sum256([]byte("123456789"))
	lowerHex := hex.EncodeToString(contentDigest[:])
	upperHex := strings.ToUpper(lowerHex)
	want, err := service.BuildCreateUploadReplacementRequestHash(31, 4, 9, contentDigest)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		fmt.Sprintf(`{"expected_revision":"4","size_bytes":9,"sha256":%q}`, lowerHex),
		fmt.Sprintf(`{ "sha256": %q, "size_bytes": 9, "expected_revision": "4" }`, upperHex),
	} {
		var request dto.CreateUploadReplacementRequest
		if err := json.Unmarshal([]byte(body), &request); err != nil {
			t.Fatal(err)
		}
		input, fieldError := dto.ParseCreateUploadReplacementRequest(request)
		if fieldError != nil {
			t.Fatalf("合法正文解析失败 %+v", fieldError)
		}
		actual, err := service.BuildCreateUploadReplacementRequestHash(31, input.ExpectedRevision, input.SizeBytes, input.ExpectedSHA256)
		if err != nil || actual != want {
			t.Fatalf("等价声明得到不同摘要 %x，错误 %v", actual, err)
		}
	}

	// 2. 每次只改变一项声明，核对请求摘要随之改变。
	for _, fixture := range []struct {
		name                         string
		entryID, revision, sizeBytes uint64
		digest                       [sha256.Size]byte
	}{
		{"target file", 32, 4, 9, contentDigest},
		{"expected revision", 31, 5, 9, contentDigest},
		{"size", 31, 4, 10, contentDigest},
		{"content checksum", 31, 4, 9, sha256.Sum256([]byte("987654321"))},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			actual, err := service.BuildCreateUploadReplacementRequestHash(fixture.entryID, fixture.revision, fixture.sizeBytes, fixture.digest)
			if err != nil || actual == want {
				t.Fatalf("改变声明未得到不同摘要 %x，错误 %v", actual, err)
			}
		})
	}
}

func TestBuildCreateUploadReplacementRequestHashRejectsInvalidDeclarations(t *testing.T) {
	for _, fixture := range []struct {
		name                         string
		entryID, revision, sizeBytes uint64
	}{
		{"zero target", 0, 4, 0},
		{"zero revision", 31, 0, 0},
		{"unsafe size", 31, 4, 1 << 53},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			actual, err := service.BuildCreateUploadReplacementRequestHash(fixture.entryID, fixture.revision, fixture.sizeBytes, sha256.Sum256(nil))
			if err == nil || actual != [sha256.Size]byte{} {
				t.Fatalf("无效声明得到摘要 %x，错误 %v", actual, err)
			}
		})
	}
}
