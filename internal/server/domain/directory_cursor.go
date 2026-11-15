package domain

import "errors"

// ErrInvalidDirectoryCursor 表示目录游标的标识、位置或编码数据无效。
var ErrInvalidDirectoryCursor = errors.New("目录分页游标无效")
