package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestStopServerMultipartUploadInitializationWorkerWaitsForActiveRound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := make(chan struct{})
		cancelled := make(chan struct{})
		finishRound := make(chan struct{})
		stopped := make(chan struct{})
		var logs bytes.Buffer
		stop := startServerMultipartUploadInitializationWorker(t.Context(), func(ctx context.Context, limit int, owner string) (service.MultipartUploadInitializationBatchResult, error) {
			if limit != 10 || len(owner) > 64 || !strings.HasPrefix(owner, "multipart-init-") {
				t.Error("worker 未提供预期的扫描上限和租约持有者", limit, owner)
			}
			if _, err := uuid.Parse(strings.TrimPrefix(owner, "multipart-init-")); err != nil {
				t.Error("worker 的租约持有者缺少独立标识", err)
			}
			close(started)
			<-ctx.Done()
			close(cancelled)
			<-finishRound
			return service.MultipartUploadInitializationBatchResult{
				Scanned: 3, Bound: 1,
				CandidateErrors: []service.MultipartUploadInitializationCandidateError{
					{TaskID: 19, Err: ctx.Err()},
					{TaskID: 20, Err: domain.ErrUploadTaskBusy},
				},
			}, ctx.Err()
		}, logging.New(&logs))
		<-started
		go func() {
			stop()
			close(stopped)
		}()
		<-cancelled
		synctest.Wait()
		select {
		case <-stopped:
			t.Error("在途轮次尚未结束，停止方法已返回")
		default:
		}
		close(finishRound)
		<-stopped
		stop()

		logged := logs.String()
		report := strings.Index(logged, "分片上传初始化轮次结束")
		exit := strings.Index(logged, "分片上传初始化 worker 已停止")
		if report < 0 || exit <= report || strings.Contains(logged, "分片上传初始化轮次失败") {
			t.Fatal("停止未完成在途汇报，或把正常取消记为轮次失败", logged)
		}
		if strings.Count(logged, "分片上传初始化任务处理失败") != 1 || !strings.Contains(logged, `"task_id":20`) ||
			!strings.Contains(logged, `"error_code":"UPLOAD_BUSY"`) || !strings.Contains(logged, `"bound":1`) {
			t.Fatal("停止前未汇报已绑定结果和独立故障", logged)
		}
	})
}

func TestServerMultipartUploadInitializationWorkerReportsRounds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const privateDetail = "fixture://private/objects/hidden?session=hidden"
		var logs bytes.Buffer
		var firstOwner string
		var startedAt time.Time
		rounds := 0
		stop := startServerMultipartUploadInitializationWorker(t.Context(), func(_ context.Context, _ int, owner string) (service.MultipartUploadInitializationBatchResult, error) {
			rounds++
			if rounds == 1 {
				firstOwner, startedAt = owner, time.Now()
				return service.MultipartUploadInitializationBatchResult{}, errors.New(privateDetail)
			}
			if owner != firstOwner || time.Since(startedAt) != 5*time.Second {
				t.Error("下一轮的租约持有者或开始时间不符", owner, time.Since(startedAt))
			}
			return service.MultipartUploadInitializationBatchResult{
				Scanned: 4, Bound: 1, RetriesScheduled: 1,
				CandidateErrors: []service.MultipartUploadInitializationCandidateError{
					{TaskID: 19, Err: fmt.Errorf("%s: %w", privateDetail, domain.ErrUploadStorageUnavailable)},
					{TaskID: math.MaxUint64, Err: errors.New(privateDetail)},
				},
			}, nil
		}, logging.New(&logs))
		synctest.Wait()
		time.Sleep(5 * time.Second)
		synctest.Wait()
		stop()

		type event struct {
			Message         string `json:"msg"`
			Code            string `json:"error_code"`
			TaskID          uint64 `json:"task_id"`
			Interval        int    `json:"interval_ms"`
			Limit           int    `json:"batch_limit"`
			Scanned         int    `json:"scanned"`
			Bound           int    `json:"bound"`
			Retries         int    `json:"retries_scheduled"`
			CandidateErrors int    `json:"candidate_errors"`
		}
		var events []event
		logged := logs.String()
		for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
			var entry event
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatal(err)
			}
			events = append(events, entry)
		}
		want := []event{
			{Message: "分片上传初始化 worker 已启动", Interval: 5000, Limit: 10},
			{Message: "分片上传初始化轮次失败", Code: "INTERNAL_ERROR"},
			{Message: "分片上传初始化任务处理失败", TaskID: 19, Code: "STORAGE_UNAVAILABLE"},
			{Message: "分片上传初始化任务处理失败", TaskID: math.MaxUint64, Code: "INTERNAL_ERROR"},
			{Message: "分片上传初始化轮次结束", Scanned: 4, Bound: 1, Retries: 1, CandidateErrors: 2},
			{Message: "分片上传初始化 worker 已停止"},
		}
		if rounds != 2 || strings.Contains(logged, privateDetail) || len(events) != len(want) {
			t.Fatal("轮次数、日志条数或脱敏结果不符", rounds, logged)
		}
		for index, entry := range events {
			if entry != want[index] {
				t.Fatalf("第 %d 条日志不符，预期 %+v，得到 %+v", index+1, want[index], entry)
			}
		}
	})
}
