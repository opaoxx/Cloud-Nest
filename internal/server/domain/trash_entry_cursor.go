package domain

import "errors"

// MaxTrashEntryCursorLength 是回收站游标文本的字节上限。
const MaxTrashEntryCursorLength = 1024

// ErrInvalidTrashEntryCursor 表示回收站游标的用户、位置或文本无效。
var ErrInvalidTrashEntryCursor = errors.New("回收站分页游标无效")
