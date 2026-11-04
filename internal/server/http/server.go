package http

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Serve 启动 HTTP 服务，并在 ctx 取消后等待在途请求结束。
// shutdownTimeout 必须大于零；关闭超过期限时强制关闭连接并返回错误。
// 正常关闭返回 nil，数据库等其他资源由调用方在本方法返回后关闭。
func Serve(ctx context.Context, httpServer *http.Server, shutdownTimeout time.Duration, logger *slog.Logger) error {
	// 1. 检查上下文，启动 HTTP 服务并接收运行结果。
	if err := ctx.Err(); err != nil {
		return err
	}
	if shutdownTimeout <= 0 {
		return fmt.Errorf("HTTP 关闭等待时限必须大于零")
	}
	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- httpServer.ListenAndServe()
	}()

	// 2. 等待服务出错或收到停止请求。
	select {
	case err := <-serveErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("HTTP 监听失败: %w", err)
	case <-ctx.Done():
		logger.Info("收到停止请求，开始关闭 HTTP 服务")
	}

	// 3. 使用独立的超时上下文，等待在途请求结束。（因为原ctx因为收到停止请求而取消，所以不能直接使用）
	shutdownContext, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	shutdownError := httpServer.Shutdown(shutdownContext)

	// 4. 超时时强制关闭连接，确认监听退出后返回。
	if shutdownError != nil {
		closeError := httpServer.Close()
		<-serveErrors
		return errors.Join(fmt.Errorf("HTTP 优雅退出失败，已强制关闭连接: %w", shutdownError), closeError)
	}
	if err := <-serveErrors; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("HTTP 监听失败: %w", err)
	}
	logger.Info("HTTP 服务已停止")
	return nil
}
