package service

import (
	"encoding/json"
	"strconv"
)

// encodeEntryRevisionRequest 按固定顺序编码回收或恢复的请求参数。
func encodeEntryRevisionRequest(operation string, entryID, expectedRevision uint64) ([]byte, error) {
	// 1. 用十进制字符串保留两个 uint64 的完整精度。
	request := struct {
		Operation        string `json:"operation"`
		EntryID          string `json:"entry_id"`
		ExpectedRevision string `json:"expected_revision"`
	}{
		Operation:        operation,
		EntryID:          strconv.FormatUint(entryID, 10),
		ExpectedRevision: strconv.FormatUint(expectedRevision, 10),
	}
	// 2. 以固定的字段顺序生成摘要的输入字节。
	return json.Marshal(request)
}
