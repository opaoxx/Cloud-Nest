package domain

import "errors"

// ErrInvalidEntryName 表示普通目录项名称未满足统一命名规则。
var ErrInvalidEntryName = errors.New("目录项名称不符合命名规则")

// ErrEntryNameConflict 表示同一用户、同一父目录下已有活动同名项。
var ErrEntryNameConflict = errors.New("当前文件夹中已存在同名文件或文件夹")
