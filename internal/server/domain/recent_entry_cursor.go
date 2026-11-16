package domain

import "errors"

// MaxRecentEntryCursorLength 限定版本 1 最近使用游标文本的字节数。
const MaxRecentEntryCursorLength = 1024

// ErrInvalidRecentEntryCursor 表示最近使用游标的用户、查询、位置或文本数据无效。
var ErrInvalidRecentEntryCursor = errors.New("最近使用分页游标无效")
