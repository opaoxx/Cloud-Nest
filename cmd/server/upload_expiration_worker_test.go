package main

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestServerUploadExpirationWorkerScheduleAndStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 第一轮完成，第二轮收到取消后还要等待清理结束。
		started := make(chan struct{})
		canceled := make(chan struct{})
		released := make(chan struct{})
		stopped := make(chan struct{})
		var logs bytes.Buffer
		var rounds atomic.Int32
		stop := startServerUploadExpirationWorker(t.Context(), func(ctx context.Context, limit int) (service.UploadExpirationBatchResult, error) {
			if limit != 100 {
				t.Error("单文件过期批量上限错误", limit)
			}
			if rounds.Add(1) == 1 {
				return service.UploadExpirationBatchResult{Scanned: 1, Expired: 1}, nil
			}
			close(started)
			<-ctx.Done()
			close(canceled)
			<-released
			return service.UploadExpirationBatchResult{}, ctx.Err()
		}, logging.New(&logs))

		// 2. 用虚拟时钟核对首轮立即开始及后续间隔。
		synctest.Wait()
		if count := rounds.Load(); count != 1 {
			t.Error("首轮未立即执行", count)
		}
		time.Sleep(30*time.Second - time.Millisecond)
		synctest.Wait()
		if count := rounds.Load(); count != 1 {
			t.Error("过期处理在间隔结束前就开始下一轮", count)
		}
		time.Sleep(time.Millisecond)
		<-started

		// 3. 停止方法发出取消，并等待当前轮次退出。
		go func() {
			stop()
			close(stopped)
		}()
		<-canceled
		synctest.Wait()
		select {
		case <-stopped:
			t.Error("当前过期处理还未结束，停止方法已经返回")
		default:
		}
		close(released)
		<-stopped
		stop()
		logged := logs.String()
		report := strings.Index(logged, "上传过期轮次结束")
		exit := strings.Index(logged, "上传过期 worker 已停止")
		if count := rounds.Load(); count != 2 || report < 0 || exit <= report || !strings.Contains(logged, `"expired":1`) || strings.Contains(logged, "上传过期轮次失败") {
			t.Fatal("过期汇报、关停顺序或取消日志不符", count, logged)
		}
	})
}
