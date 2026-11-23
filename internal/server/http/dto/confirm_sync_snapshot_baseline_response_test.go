package dto

import (
	"encoding/json"
	"math"
	"reflect"
	"strconv"
	"testing"
)

func TestNewConfirmSyncSnapshotBaselineDataJSON(t *testing.T) {
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	for _, testCase := range []struct {
		name     string
		position uint64
		want     string
	}{
		{"zero baseline", 0, `"0"`},
		{"confirmed position", 17, `"17"`},
		{"above JavaScript safe integer", 9007199254740993, `"9007199254740993"`},
		{"maximum unsigned position", math.MaxUint64, `"18446744073709551615"`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 编码完整响应，核对外层字段和请求 ID。
			response := ConfirmSyncSnapshotBaselineResponse{
				Data: NewConfirmSyncSnapshotBaselineData(testCase.position), RequestID: requestID,
			}
			encoded, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &body); err != nil || len(body) != 2 || string(body["request_id"]) != strconv.Quote(requestID) {
				t.Fatalf("成功响应外层字段不符：%s，错误 %v", encoded, err)
			}

			// 2. 核对公开字段精确匹配，序号明确编码为字符串。
			var data map[string]json.RawMessage
			want := map[string]json.RawMessage{
				"status":       json.RawMessage(`"ACTIVE"`),
				"ack_sequence": json.RawMessage(testCase.want),
			}
			if err := json.Unmarshal(body["data"], &data); err != nil || !reflect.DeepEqual(data, want) {
				t.Fatalf("确认数据字段或类型不符：%s，错误 %v", body["data"], err)
			}
		})
	}
}
