package http

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
)

func TestCreateUploadReplacementRequestHTTP(t *testing.T) {
	const maxUint64Text = "18446744073709551615"
	const maxSafeSize = 9007199254740991
	sampleDigest := sha256.Sum256([]byte("123456789"))
	sampleHex := hex.EncodeToString(sampleDigest[:])
	emptyDigest := sha256.Sum256(nil)
	emptyHex := hex.EncodeToString(emptyDigest[:])
	bodyFor := func(revisionJSON, sizeJSON, digestJSON string) string {
		return fmt.Sprintf(`{"expected_revision":%s,"size_bytes":%s,"sha256":%s}`, revisionJSON, sizeJSON, digestJSON)
	}
	validBody := bodyFor(`"4"`, "9", fmt.Sprintf("%q", sampleHex))
	for _, fixture := range []struct {
		name, pathID, body, field   string
		expectedID, revision, bytes uint64
		digest                      [sha256.Size]byte
	}{
		{name: "OpenAPI example", pathID: "31", body: validBody, expectedID: 31, revision: 4, bytes: 9, digest: sampleDigest},
		{name: "explicit zero with empty file checksum", pathID: "1", body: bodyFor(`"1"`, "0", fmt.Sprintf("%q", emptyHex)),
			expectedID: 1, revision: 1, digest: emptyDigest},
		{name: "single boundary with uppercase checksum", pathID: "31", body: bodyFor(`"4"`, "16777216", fmt.Sprintf("%q", strings.ToUpper(sampleHex))),
			expectedID: 31, revision: 4, bytes: domain.MaxSingleUploadSizeBytes, digest: sampleDigest},
		{name: "multipart boundary", pathID: "31", body: bodyFor(`"4"`, "16777217", fmt.Sprintf("%q", sampleHex)),
			expectedID: 31, revision: 4, bytes: domain.MaxSingleUploadSizeBytes + 1, digest: sampleDigest},
		{name: "maximum ID revision and safe size", pathID: maxUint64Text, body: bodyFor(`"18446744073709551615"`, "9007199254740991", fmt.Sprintf("%q", sampleHex)),
			expectedID: ^uint64(0), revision: ^uint64(0), bytes: maxSafeSize, digest: sampleDigest},
		{name: "leading zero ID before body", pathID: "031", body: "{", field: "id"},
		{name: "overflow ID", pathID: "18446744073709551616", body: validBody, field: "id"},
		{name: "unknown request identity", pathID: "31", body: strings.TrimSuffix(validBody, "}") + `,"user_id":"2"}`},
		{name: "second JSON object", pathID: "31", body: validBody + "{}"},
		{name: "array body", pathID: "31", body: "[]"},
		{name: "malformed body", pathID: "31", body: "{"},
		{name: "missing revision before other fields", pathID: "31", body: "{}", field: "expected_revision"},
		{name: "null revision", pathID: "31", body: bodyFor("null", "0", "null"), field: "expected_revision"},
		{name: "numeric revision", pathID: "31", body: bodyFor("4", "0", "null"), field: "expected_revision"},
		{name: "leading zero revision", pathID: "31", body: bodyFor(`"04"`, "0", "null"), field: "expected_revision"},
		{name: "zero revision", pathID: "31", body: bodyFor(`"0"`, "0", "null"), field: "expected_revision"},
		{name: "overflow revision", pathID: "31", body: bodyFor(`"18446744073709551616"`, "0", "null"), field: "expected_revision"},
		{name: "missing size before checksum", pathID: "31", body: `{"expected_revision":"4"}`, field: "size_bytes"},
		{name: "null size", pathID: "31", body: bodyFor(`"4"`, "null", "null"), field: "size_bytes"},
		{name: "negative size", pathID: "31", body: bodyFor(`"4"`, "-1", "null"), field: "size_bytes"},
		{name: "quoted size", pathID: "31", body: bodyFor(`"4"`, `"9"`, "null"), field: "size_bytes"},
		{name: "fractional size", pathID: "31", body: bodyFor(`"4"`, "9.5", "null"), field: "size_bytes"},
		{name: "exponent size", pathID: "31", body: bodyFor(`"4"`, "9e1", "null"), field: "size_bytes"},
		{name: "unsafe integer size", pathID: "31", body: bodyFor(`"4"`, "9007199254740992", "null"), field: "size_bytes"},
		{name: "missing single checksum", pathID: "31", body: `{"expected_revision":"4","size_bytes":0}`, field: "sha256"},
		{name: "null multipart checksum", pathID: "31", body: bodyFor(`"4"`, "16777217", "null"), field: "sha256"},
		{name: "numeric checksum", pathID: "31", body: bodyFor(`"4"`, "9", "123"), field: "sha256"},
		{name: "short checksum", pathID: "31", body: bodyFor(`"4"`, "9", fmt.Sprintf("%q", sampleHex[:63])), field: "sha256"},
		{name: "non-hex checksum", pathID: "31", body: bodyFor(`"4"`, "9", fmt.Sprintf("%q", "g"+sampleHex[1:])), field: "sha256"},
		{name: "checksum with whitespace", pathID: "31", body: bodyFor(`"4"`, "9", fmt.Sprintf("%q", sampleHex+" ")), field: "sha256"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 在内存中按路径、共享 JSON 读取和正文校验的顺序执行。
			response := httptest.NewRecorder()
			requestContext, _ := gin.CreateTestContext(response)
			requestContext.Request = httptest.NewRequest(http.MethodPost, "/test-content-replacement", strings.NewReader(fixture.body))
			requestContext.Request.Header.Set("Content-Type", "application/json")
			var acceptedPath *dto.EntryPath
			var acceptedInput *dto.CreateUploadReplacementInput
			func() {
				path, fieldError := dto.ParseEntryPath(fixture.pathID)
				if fieldError != nil {
					handler.InvalidArgument(requestContext, fieldError)
					return
				}
				var request dto.CreateUploadReplacementRequest
				if !binding.ReadJSON(requestContext, &request) {
					return
				}
				input, fieldError := dto.ParseCreateUploadReplacementRequest(request)
				if fieldError != nil {
					handler.InvalidArgument(requestContext, fieldError)
					return
				}
				acceptedPath, acceptedInput = path, input
				requestContext.AbortWithStatus(http.StatusNoContent)
			}()

			// 2. 核对完整数值、规范化摘要和首个错误字段。
			if fixture.expectedID != 0 {
				want := dto.CreateUploadReplacementInput{
					ExpectedRevision: fixture.revision, SizeBytes: fixture.bytes, ExpectedSHA256: fixture.digest,
				}
				if response.Code != http.StatusNoContent || acceptedPath == nil || acceptedPath.EntryID != fixture.expectedID ||
					acceptedInput == nil || *acceptedInput != want {
					t.Fatalf("解析结果不符，状态 %d，路径 %+v，正文 %+v", response.Code, acceptedPath, acceptedInput)
				}
				return
			}
			var failure dto.ErrorResponse
			if response.Code != http.StatusBadRequest || acceptedPath != nil || acceptedInput != nil ||
				json.Unmarshal(response.Body.Bytes(), &failure) != nil || failure.Error.Code != "INVALID_ARGUMENT" {
				t.Fatalf("拒绝结果不符，状态 %d，正文 %s", response.Code, response.Body.String())
			}
			if fixture.field == "" && failure.Error.Details != nil ||
				fixture.field != "" && (len(failure.Error.Details) != 1 || failure.Error.Details["field"] != fixture.field) {
				t.Fatalf("预期定位 %q，收到 %+v", fixture.field, failure.Error.Details)
			}
		})
	}
}
