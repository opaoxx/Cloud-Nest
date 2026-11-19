package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestServerUploadCancellationWorkerReportsFailureAndWaitsForStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 注入一次扫描失败，让下一轮停在清理过程中。
		started := make(chan time.Time, 2)
		canceled := make(chan struct{})
		finish := make(chan struct{})
		stopped := make(chan struct{})
		var logs bytes.Buffer
		const privateError = "fixture-private-database-error"
		startedAt := time.Now()
		rounds := 0
		stop := startServerUploadCancellationWorker(t.Context(), func(ctx context.Context, limit int) (service.UploadCancellationBatchResult, error) {
			if limit != 100 {
				t.Error("取消批量上限不符", limit)
			}
			rounds++
			started <- time.Now()
			if rounds == 1 {
				return service.UploadCancellationBatchResult{}, errors.New(privateError)
			}
			<-ctx.Done()
			close(canceled)
			<-finish
			return service.UploadCancellationBatchResult{Scanned: 1, Cancelled: 1}, ctx.Err()
		}, logging.New(&logs))

		// 2. 首轮立即开始，扫描失败后按 30 秒间隔继续。
		if first, second := <-started, <-started; !first.Equal(startedAt) || second.Sub(first) != 30*time.Second {
			t.Error("首轮与下一轮的执行时间不符", first.Sub(startedAt), second.Sub(first))
		}
		go func() {
			stop()
			close(stopped)
		}()
		<-canceled
		synctest.Wait()
		select {
		case <-stopped:
			t.Error("当前轮次尚未退出，停止方法已经返回")
		default:
		}

		// 3. 清理完成后关闭 worker，核对故障和关停时的汇报。
		close(finish)
		<-stopped
		stop()
		logged := logs.String()
		report := strings.Index(logged, "上传取消轮次结束")
		exit := strings.Index(logged, "上传取消 worker 已停止")
		if strings.Count(logged, "上传取消轮次失败") != 1 || strings.Contains(logged, privateError) ||
			report < 0 || exit <= report || !strings.Contains(logged, `"scanned":1,"cancelled":1,"candidate_errors":0`) {
			t.Fatal("取消 worker 的故障或关停汇报不符", logged)
		}
	})
}
