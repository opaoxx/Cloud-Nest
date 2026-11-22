package dto

import (
	"bytes"
	"encoding/json"
	"io"
)

// readUniqueJSONObject 读取一个 JSON 对象，并拒绝重复字段和追加内容。
func readUniqueJSONObject(body []byte) (map[string]json.RawMessage, bool) {
	// 1. 解码对象，并在键解码后检查重复，覆盖转义写法。
	decoder := json.NewDecoder(bytes.NewReader(body))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, false
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		key, isString := token.(string)
		if _, duplicate := fields[key]; err != nil || !isString || duplicate {
			return nil, false
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, false
		}
		fields[key] = value
	}

	// 2. 确认对象闭合且正文到此结束。
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, false
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, false
	}
	return fields, true
}
