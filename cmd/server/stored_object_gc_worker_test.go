package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestServerStoredObjectGCWorkerReportsAndWaitsForStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 首轮失败，第二轮开始执行并等待关停。
		var logs bytes.Buffer
		started := make(chan struct{})
		canceled := make(chan struct{})
		release := make(chan struct{})
		stopped := make(chan struct{})
		rounds := 0
		start := time.Now()
		stop := startServerStoredObjectGCWorker(t.Context(), func(ctx context.Context, recoveryLimit, dueLimit int) (service.StoredObjectGCRoundResult, error) {
			if recoveryLimit != 100 || dueLimit != 100 {
				t.Error("两个批次应各最多扫描 100 项", recoveryLimit, dueLimit)
			}
			rounds++
			if rounds == 1 {
				if time.Since(start) != 0 {
					t.Error("第一轮应立即执行")
				}
				return service.StoredObjectGCRoundResult{}, errors.New("fixture private scan detail")
			}
			close(started)
			<-ctx.Done()
			close(canceled)
			<-release
			time.Sleep(2 * time.Second)
			return service.StoredObjectGCRoundResult{
				Recovery: service.StoredObjectDeletionRecoveryResult{
					Scanned: 2, Completed: 1,
					CandidateErrors: []service.StoredObjectDeletionCandidateError{
						{ObjectID: 31, OwnerUserID: 7, Err: fmt.Errorf("fixture private recovery detail: %w", domain.ErrStoredObjectDeleteUnavailable)},
					},
				},
				Due: service.StoredObjectGCBatchResult{
					Scanned: 4, Completed: 1, StaleCandidates: 1,
					CandidateErrors: []service.StoredObjectDeletionCandidateError{
						{ObjectID: 32, OwnerUserID: 7, Err: fmt.Errorf("fixture private reference detail: %w", domain.ErrStoredObjectInUse)},
						{ObjectID: 33, OwnerUserID: 8, Err: ctx.Err()},
					},
				},
			}, fmt.Errorf("fixture private cancellation detail: %w", ctx.Err())
		}, logging.New(&logs))
		<-started
		if time.Since(start) != time.Minute {
			t.Error("第二轮应在第 60 秒执行", time.Since(start))
		}

		// 2. 发出停止信号，验证当前轮次返回之前停止方法一直等待。
		go func() {
			stop()
			close(stopped)
		}()
		<-canceled
		synctest.Wait()
		select {
		case <-stopped:
			t.Error("当前回收轮尚未结束，停止方法已返回")
		default:
		}
		close(release)
		<-stopped
		stop()

		// 3. 两阶段结果都已汇报，关停错误只计入汇总，原始错误细节不进入日志。
		logText := logs.String()
		report := strings.Index(logText, "对象回收轮次结束")
		exit := strings.Index(logText, "对象回收 worker 已停止")
		if rounds != 2 || report < 0 || exit <= report ||
			strings.Count(logText, "对象回收轮次失败") != 1 || strings.Count(logText, "存储对象回收失败") != 2 ||
			strings.Contains(logText, "fixture private") || strings.Contains(logText, "对象回收 worker 异常退出") ||
			strings.Contains(logText, `"object_id":33`) {
			t.Fatal("轮次日志和退出顺序不符", rounds, logText)
		}
		for _, field := range []string{
			`"interval_ms":60000,"recovery_limit":100,"due_limit":100`,
			`"error_code":"INTERNAL_ERROR"`,
			`"phase":"recovery","user_id":7,"object_id":31,"error_code":"STORED_OBJECT_DELETE_UNAVAILABLE"`,
			`"phase":"due","user_id":7,"object_id":32,"error_code":"STORED_OBJECT_IN_USE"`,
			`"duration_ms":2000`,
			`"recovery":{"scanned":2,"completed":1,"candidate_errors":1}`,
			`"due":{"scanned":4,"completed":1,"stale_candidates":1,"candidate_errors":2}`,
		} {
			if !strings.Contains(logText, field) {
				t.Errorf("日志缺少 %s\n%s", field, logText)
			}
		}
	})
}

func TestServerStoredObjectGCWorkerStopsWithServiceContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 让首轮等待服务级停止信号。
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		started := make(chan struct{})
		canceled := make(chan struct{})
		var logs bytes.Buffer
		stop := startServerStoredObjectGCWorker(ctx, func(roundContext context.Context, _, _ int) (service.StoredObjectGCRoundResult, error) {
			close(started)
			<-roundContext.Done()
			close(canceled)
			return service.StoredObjectGCRoundResult{}, roundContext.Err()
		}, logging.New(&logs))
		<-started

		// 2. 上层取消应传给在途轮次，随后可等待 worker 退出。
		cancel()
		<-canceled
		stop()
		if logText := logs.String(); !strings.Contains(logText, "对象回收 worker 已停止") || strings.Contains(logText, `"level":"WARN"`) {
			t.Fatal("服务停止时的 worker 日志不符", logText)
		}
	})
}
