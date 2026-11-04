// Package logging 提供服务端日志的统一初始化入口。
package logging

import (
	"io"
	"log/slog"
)

// New 创建写入 output 的 JSON 日志对象，记录 INFO 及以上级别的日志。
// 每条日志附带 service=cloudnest-api，output 由调用方提供和管理。
func New(output io.Writer) *slog.Logger {
	// 1. 创建 JSON 日志处理器，将日志写入指定位置。
	handler := slog.NewJSONHandler(output, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})

	// 2. 创建日志对象，统一附加服务名称。
	logger := slog.New(handler).With("service", "cloudnest-api")

	// 3. 返回日志对象，交给启动入口使用。
	return logger
}
