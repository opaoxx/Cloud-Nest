package domain

import "errors"

// ErrRootEntryTrash 表示用户的真实根目录禁止回收。
var ErrRootEntryTrash = errors.New("根目录不能回收")
