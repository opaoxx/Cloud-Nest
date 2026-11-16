package domain

import "errors"

// MaxSizeEntryCursorLength 限定大小游标文本的最大字节数。
const MaxSizeEntryCursorLength = 1024

// ErrInvalidSizeEntryCursor 表示大小游标的用户、查询、位置或文本数据无效。
var ErrInvalidSizeEntryCursor = errors.New("大小分页游标无效")
