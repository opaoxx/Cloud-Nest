package domain

import "errors"

// ErrSyncDeviceNotFound 表示归属正确的设备尚未建立同步记录。
var ErrSyncDeviceNotFound = errors.New("设备同步记录不存在")

// ErrInvalidSyncDeviceData 表示同步设备的归属或代次数据异常。
var ErrInvalidSyncDeviceData = errors.New("设备同步数据异常")
