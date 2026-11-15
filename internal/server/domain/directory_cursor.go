package domain

import "errors"

// MaxDirectoryCursorLength 限定版本 1 游标输入的字节数，覆盖当前合法载荷。
const MaxDirectoryCursorLength = 1024

// ErrInvalidDirectoryCursor 表示目录游标的标识、位置或编码数据无效。
var ErrInvalidDirectoryCursor = errors.New("目录分页游标无效")
