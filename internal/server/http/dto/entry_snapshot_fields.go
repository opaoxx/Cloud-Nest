package dto

import (
	"bytes"
	"encoding/json"
	"io"
	"unicode/utf8"
)

// readEntrySnapshotFields 从 entry 对象读取指定数量的字段，可指定一个 JSON 数字字段。
func readEntrySnapshotFields(body []byte, fieldCount int, numberField string) (map[string]string, bool) {
	// 1. 检查 UTF-8 与只有 entry 对象的外层。
	if !utf8.Valid(body) {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	for _, expected := range []json.Token{json.Delim('{'), "entry", json.Delim('{')} {
		token, err := decoder.Token()
		if err != nil || token != expected {
			return nil, false
		}
	}

	// 2. 读取 entry 的字段，数字保留原始文本供调用方校验。
	fields, valid := readSnapshotFields(decoder, fieldCount, numberField)
	if !valid {
		return nil, false
	}

	// 3. 检查两个对象完整闭合，正文到此结束。
	for range 2 {
		token, err := decoder.Token()
		if err != nil || token != json.Delim('}') {
			return nil, false
		}
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, false
	}
	return fields, true
}

// readSnapshotFields 读取已打开对象的非空标量字段，闭合符由调用方读取。
func readSnapshotFields(decoder *json.Decoder, fieldCount int, numberField string) (map[string]string, bool) {
	// 1. 允许字段调序，拒绝解码后重复的键以及不匹配的值类型。
	fields := make(map[string]string, fieldCount)
	for decoder.More() {
		nameToken, nameError := decoder.Token()
		name, validName := nameToken.(string)
		if _, duplicate := fields[name]; nameError != nil || !validName || duplicate || len(fields) == fieldCount {
			return nil, false
		}
		valueToken, valueError := decoder.Token()
		if valueError != nil {
			return nil, false
		}
		if numberField != "" && name == numberField {
			number, validNumber := valueToken.(json.Number)
			if !validNumber {
				return nil, false
			}
			fields[name] = number.String()
			continue
		}
		value, validValue := valueToken.(string)
		if !validValue || value == "" {
			return nil, false
		}
		fields[name] = value
	}
	// 2. 确认实际字段数量符合快照结构。
	if len(fields) != fieldCount {
		return nil, false
	}
	return fields, true
}
