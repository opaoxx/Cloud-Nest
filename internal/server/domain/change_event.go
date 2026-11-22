package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

// ErrInvalidChangeEventInput 表示待写事件的标识、类型或载荷格式无效。
var ErrInvalidChangeEventInput = errors.New("变更事件输入无效")

// ErrChangeSequenceExhausted 表示账号变更序号无法继续递增。
var ErrChangeSequenceExhausted = errors.New("账号变更序号已达上限")

// ChangeEventInput 保存业务事务已核对的单条变更事实。
type ChangeEventInput struct {
	EntryID   uint64
	Revision  uint64
	EventType string
	Payload   json.RawMessage
}

// IsValid 检查入库前的通用字段格式，具体事件字段由各业务调用方校验。
func (input ChangeEventInput) IsValid() bool {
	// 1. 校验非零标识和 ASCII 事件类型。
	if input.EntryID == 0 || input.Revision == 0 || len(input.EventType) == 0 || len(input.EventType) > 32 ||
		strings.ContainsFunc(input.EventType, func(character rune) bool { return character < 0x21 || character > 0x7e }) {
		return false
	}
	// 2. 载荷须是有效 UTF-8 的 JSON 对象，允许前导 JSON 空白。
	payload := bytes.TrimLeft(input.Payload, " \t\r\n")
	return len(payload) != 0 && payload[0] == '{' && utf8.Valid(payload) && json.Valid(payload)
}
