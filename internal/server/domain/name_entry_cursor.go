package domain

import "errors"

// MaxNameEntryCursorLength 限定名称游标文本的字节数，容纳 768 字节键的可逆编码。
const MaxNameEntryCursorLength = 2048

// ErrInvalidNameEntryCursor 表示名称游标的用户、查询、位置或文本数据无效。
var ErrInvalidNameEntryCursor = errors.New("名称分页游标无效")
