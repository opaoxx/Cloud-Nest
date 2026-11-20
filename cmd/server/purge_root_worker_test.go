package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestServerPurgeRootWorkerReportsAndWaitsForStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 首轮扫描报错，下一轮收到取消后等待当前处理收尾。
		var logs bytes.Buffer
		started := make(chan struct{})
		canceled := make(chan struct{})
		release := make(chan struct{})
		stopped := make(chan struct{})
		rounds := 0
		stop := startServerPurgeRootWorker(t.Context(), func(ctx context.Context, rootLimit, leafLimit int) (service.PurgeRootBatchResult, error) {
			if rootLimit != 100 || leafLimit != 100 {
				t.Error("永久清理的两个批量上限不符", rootLimit, leafLimit)
			}
			rounds++
			if rounds == 1 {
				return service.PurgeRootBatchResult{}, errors.New("private purge scan detail")
			}
			close(started)
			<-ctx.Done()
			close(canceled)
			<-release
			return service.PurgeRootBatchResult{Scanned: 3, CompletedLeaves: 1, StaleRoots: 1,
				CandidateErrors: []service.PurgeRootCandidateError{
					{UserID: 7, RootID: 19, Err: fmt.Errorf("private purge account detail: %w", domain.ErrInvalidAccountData)},
					{UserID: 8, RootID: 20, Err: ctx.Err()},
				}}, ctx.Err()
		}, logging.New(&logs))
		<-started

		// 2. 停止方法发出取消，并等当前轮次真正返回。
		go func() {
			stop()
			close(stopped)
		}()
		<-canceled
		synctest.Wait()
		select {
		case <-stopped:
			t.Error("当前永久清理尚未结束，停止方法已返回")
		default:
		}
		close(release)
		<-stopped
		stop()

		// 3. 扫描失败和独立根项错误使用固定码，保留汇总并略过关停告警。
		logText := logs.String()
		report := strings.Index(logText, "永久清理轮次结束")
		exit := strings.Index(logText, "永久清理 worker 已停止")
		if rounds != 2 || report < 0 || exit <= report ||
			!strings.Contains(logText, `"interval_ms":5000,"root_limit":100,"leaf_limit":100`) ||
			!strings.Contains(logText, `"scanned":3,"completed_leaves":1,"stale_roots":1,"candidate_errors":2`) ||
			strings.Count(logText, "永久清理轮次失败") != 1 || !strings.Contains(logText, `"error_code":"INTERNAL_ERROR"`) ||
			strings.Count(logText, "永久清理根项处理失败") != 1 ||
			!strings.Contains(logText, `"user_id":7,"root_id":19,"error_code":"INVALID_ACCOUNT_DATA"`) ||
			strings.Contains(logText, "private purge") || strings.Contains(logText, "永久清理 worker 异常退出") {
			t.Fatal("永久清理日志或停止顺序不符", rounds, logText)
		}
	})
}
