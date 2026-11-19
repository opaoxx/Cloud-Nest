package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestStopServerUploadVerificationWorkerWaitsForActiveRound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 模拟收到取消后还需要做清理的一轮校验。
		started := make(chan struct{})
		canceled := make(chan struct{})
		released := make(chan struct{})
		stopped := make(chan struct{})
		var logs bytes.Buffer
		stop := startServerUploadVerificationWorker(t.Context(), func(ctx context.Context, limit int, owner string) (service.UploadVerificationBatchResult, error) {
			if limit != uploadVerificationBatchLimit || len(owner) > 64 || !strings.HasPrefix(owner, "upload-verification-") {
				t.Error("worker 未提供约定的扫描上限或租约标识", limit, owner)
			}
			close(started)
			<-ctx.Done()
			close(canceled)
			<-released
			return service.UploadVerificationBatchResult{Scanned: 1}, ctx.Err()
		}, logging.New(&logs))
		<-started

		// 2. 停止方法发出取消，仍须等本轮清理完毕。
		go func() {
			stop()
			close(stopped)
		}()
		<-canceled
		synctest.Wait()
		select {
		case <-stopped:
			t.Error("进行中的校验尚未结束，停止方法已经返回")
		default:
		}
		close(released)
		<-stopped

		// 3. 最后一次汇报在停止日志之前完成，重复停止也可直接返回。
		stop()
		logged := logs.String()
		report := strings.Index(logged, "上传校验轮次结束")
		exit := strings.Index(logged, "上传校验 worker 已停止")
		if report < 0 || exit <= report || strings.Contains(logged, "上传校验轮次失败") {
			t.Fatal("停止时未交付部分结果，或把正常取消记录为故障", logged)
		}
	})
}
