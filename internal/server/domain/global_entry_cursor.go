package domain

import "errors"

// MaxGlobalEntryCursorLength 限定版本 1 全局游标文本的字节数。
const MaxGlobalEntryCursorLength = 1024

// ErrInvalidGlobalEntryCursor 表示全局游标的用户、查询、位置或文本数据无效。
var ErrInvalidGlobalEntryCursor = errors.New("全局分页游标无效")
