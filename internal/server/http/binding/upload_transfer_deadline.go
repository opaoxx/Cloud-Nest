package binding

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

const uploadTransferTimeout = 5 * time.Minute

// WithUploadTransferDeadline 用同一个期限限制 HTTP 正文读取和传输回调。
// 在 HTTP 处理函数内、读取正文前调用。回调同步传输，由调用方处理响应。
func WithUploadTransferDeadline(ctx context.Context, writer http.ResponseWriter, transfer func(context.Context) error) error {
	// 1. 校验输入，计算本次传输的有效期限。
	if ctx == nil || writer == nil || transfer == nil {
		return fmt.Errorf("限制上传传输期限需要上下文、HTTP 写入器和传输方法")
	}
	transferContext, cancel := context.WithTimeout(ctx, uploadTransferTimeout)
	defer cancel()
	if err := transferContext.Err(); err != nil {
		return err
	}
	deadline, _ := transferContext.Deadline()

	// 2. 将同一个期限设置到 HTTP 正文读取上。
	// 保留读取期限，覆盖 net/http 写响应时可能发生的剩余正文处理。
	// 本次请求结束时，读取期限随连接或流的生命周期由 net/http 管理。
	if err := http.NewResponseController(writer).SetReadDeadline(deadline); err != nil {
		return fmt.Errorf("设置上传正文读取期限失败: %w", err)
	}
	if err := transferContext.Err(); err != nil {
		return err
	}

	// 3. 执行传输，保留原始错误并归并超时原因。
	transferError := transfer(transferContext)
	contextError := transferContext.Err()
	if !time.Now().Before(deadline) || errors.Is(transferError, os.ErrDeadlineExceeded) {
		// 网络读取超时可能先触发请求取消，仍按实际截止时间识别超时。
		contextError = context.DeadlineExceeded
	}
	if contextError != nil && !errors.Is(transferError, contextError) {
		return errors.Join(transferError, contextError)
	}
	return transferError
}
