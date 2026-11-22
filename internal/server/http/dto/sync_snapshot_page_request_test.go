package dto

import (
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const syncSnapshotRequestFixtureID = "052dc4f7-f4c1-46b3-a509-bf96992f6580"

func TestParseSyncSnapshotPageRequestPreservesDecodedCursor(t *testing.T) {
	// 1. 核对明文或百分号编码的凭据均还原为同一原文，且支持长度上限。
	maxCursor := strings.Repeat("A", domain.MaxSyncSnapshotTokenLength-2) + ".b"
	for _, testCase := range []struct{ query, cursor string }{
		{"cursor=A_b-9.C_d-8", "A_b-9.C_d-8"},
		{"%63ursor=%41%5Fb%2D9%2EC%5fd%2d8", "A_b-9.C_d-8"},
		{"cursor=A_b-9.C_d-8&limit=1", "A_b-9.C_d-8"},
		{"cursor=" + maxCursor, maxCursor},
	} {
		parsed, fieldError := ParseSyncSnapshotPageRequest(syncSnapshotRequestFixtureID, testCase.query)
		if fieldError != nil || parsed == nil || parsed.SnapshotID.String() != syncSnapshotRequestFixtureID || parsed.Cursor != testCase.cursor {
			t.Fatalf("解析没有保留快照标识和凭据原文，错误=%v", fieldError)
		}
	}
}

func TestParseSyncSnapshotPageRequestRejectsInvalidPath(t *testing.T) {
	// 1. UUID 使用标准写法，版本和变体均须匹配。
	for _, id := range []string{
		"", " " + syncSnapshotRequestFixtureID, strings.ToUpper(syncSnapshotRequestFixtureID),
		strings.ReplaceAll(syncSnapshotRequestFixtureID, "-", ""), "{" + syncSnapshotRequestFixtureID + "}",
		"urn:uuid:" + syncSnapshotRequestFixtureID, "052dc4f7-f4c1-16b3-a509-bf96992f6580",
		"052dc4f7-f4c1-46b3-0509-bf96992f6580", "00000000-0000-0000-0000-000000000000",
	} {
		if result, fieldError := ParseSyncSnapshotPageRequest(id, "cursor=AQ.Ag"); result != nil || fieldError == nil || fieldError.Field != "snapshot_id" {
			t.Fatalf("非法快照 ID 应定位到 snapshot_id，错误=%v", fieldError)
		}
	}
}

func TestParseSyncSnapshotPageRequestRejectsInvalidQuery(t *testing.T) {
	// 1. 原始查询格式错误定位到 query，解码成功后把凭据问题定位到 cursor。
	for _, testCase := range []struct{ name, query, field string }{
		{"missing", "", "cursor"},
		{"parameter without value", "cursor", "cursor"},
		{"blank", "cursor=", "cursor"},
		{"upper-case key", "Cursor=AQ.Ag", "cursor"},
		{"duplicate identical", "cursor=AQ.Ag&cursor=AQ.Ag", "cursor"},
		{"duplicate escaped key", "cursor=AQ.Ag&%63ursor=Aw.BA", "cursor"},
		{"space prefix", "cursor=%20AQ.Ag", "cursor"},
		{"plus becomes space", "cursor=AQ.Ag+", "cursor"},
		{"encoded newline", "cursor=AQ.Ag%0a", "cursor"},
		{"wrong alphabet", "cursor=AQ%2F.Ag", "cursor"},
		{"padding", "cursor=AQ.Ag%3D", "cursor"},
		{"double encoded dot", "cursor=AQ%252EAg", "cursor"},
		{"empty segment", "cursor=.Ag", "cursor"},
		{"extra segment", "cursor=AQ.Ag.Aw", "cursor"},
		{"overlong", "cursor=" + strings.Repeat("a", domain.MaxSyncSnapshotTokenLength-1) + ".b", "cursor"},
		{"malformed escape", "cursor=AQ.%GG", "query"},
		{"malformed other field beside cursor", "cursor=AQ.Ag&ignored=%", "query"},
		{"unescaped semicolon", "cursor=AQ.Ag;limit=1", "query"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result, fieldError := ParseSyncSnapshotPageRequest(syncSnapshotRequestFixtureID, testCase.query)
			if result != nil || fieldError == nil || fieldError.Field != testCase.field || fieldError.Message == "" {
				t.Fatalf("错误字段应为 %s，实际=%v", testCase.field, fieldError)
			}
		})
	}
}
