package domain

import "errors"

// ErrInvalidEntryName 表示普通目录项名称未满足统一命名规则。
var ErrInvalidEntryName = errors.New("目录项名称不符合命名规则")
