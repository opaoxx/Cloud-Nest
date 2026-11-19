package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
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
			return service.UploadVerificationBatchResult{
				Scanned: 2,
				CandidateErrors: []service.UploadVerificationCandidateError{
					{TaskID: 19, Err: ctx.Err()},
					{TaskID: 20, Err: domain.ErrUploadTaskBusy},
				},
			}, ctx.Err()
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
		if strings.Count(logged, "上传校验任务处理失败") != 1 || !strings.Contains(logged, `"task_id":20`) || !strings.Contains(logged, `"error_code":"UPLOAD_BUSY"`) {
			t.Fatal("服务停止时未保留独立故障，或记录了正常取消", logged)
		}
	})
}

func TestServerUploadVerificationWorkerLogsCandidateFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 交给正式 worker 一轮结果，包含已证实失配、已知与未知故障。
		const privateDetail = "fixture://private/object?credential=hidden"
		var logs bytes.Buffer
		stop := startServerUploadVerificationWorker(t.Context(), func(context.Context, int, string) (service.UploadVerificationBatchResult, error) {
			return service.UploadVerificationBatchResult{
				Scanned: 4, Published: 1, CandidateErrors: []service.UploadVerificationCandidateError{
					{TaskID: 17, Err: &domain.UploadVerificationSizeMismatch{}},
					{TaskID: 18, Err: fmt.Errorf("%s: %w", privateDetail, domain.ErrUploadTaskBusy)},
					{TaskID: math.MaxUint64, Err: errors.New(privateDetail)},
				},
			}, nil
		}, logging.New(&logs))
		synctest.Wait()
		stop()

		// 2. 解码真实 JSON 日志，核对完整无符号标识、固定码和脱敏边界。
		type failureLog struct {
			Message string `json:"msg"`
			Level   string `json:"level"`
			TaskID  uint64 `json:"task_id"`
			Code    string `json:"error_code"`
		}
		var actual []failureLog
		logged := logs.String()
		for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
			var entry failureLog
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatal(err)
			}
			if entry.Message == "上传校验任务处理失败" {
				actual = append(actual, entry)
			}
		}
		want := []failureLog{
			{Message: "上传校验任务处理失败", Level: "WARN", TaskID: 17, Code: "BODY_SIZE_MISMATCH"},
			{Message: "上传校验任务处理失败", Level: "WARN", TaskID: 18, Code: "UPLOAD_BUSY"},
			{Message: "上传校验任务处理失败", Level: "WARN", TaskID: math.MaxUint64, Code: "INTERNAL_ERROR"},
		}
		if !slices.Equal(actual, want) || strings.Contains(logged, privateDetail) || !strings.Contains(logged, `"candidate_errors":3`) {
			t.Fatal("逐条错误字段、脱敏或汇总错误", actual, logged)
		}
	})
}

func TestUploadVerificationErrorCode(t *testing.T) {
	for _, scenario := range []struct {
		name string
		err  error
		want string
	}{
		{"digest mismatch evidence", &domain.UploadVerificationChecksumMismatch{}, "CHECKSUM_MISMATCH"},
		{"missing target", domain.ErrUploadTargetUnavailable, "TARGET_UNAVAILABLE"},
		{"missing physical object", domain.ErrUploadObjectMissing, "UPLOAD_OBJECT_MISSING"},
		{"missing object record", domain.ErrStoredObjectNotFound, "STORED_OBJECT_NOT_FOUND"},
		{"missing task", domain.ErrUploadTaskNotFound, "UPLOAD_NOT_FOUND"},
		{"task no longer ready", domain.ErrUploadTaskNotReady, "TASK_NOT_READY"},
		{"lease version exhausted", domain.ErrUploadLeaseVersionExhausted, "TASK_NOT_READY"},
		{"file too large", domain.ErrUploadFileTooLarge, "FILE_TOO_LARGE"},
		{"task context canceled", context.Canceled, "CANCELED"},
		{"timeout", fmt.Errorf("context: %w", context.DeadlineExceeded), "DEADLINE_EXCEEDED"},
		{"size mismatch and storage fault", errors.Join(&domain.UploadVerificationSizeMismatch{}, domain.ErrUploadStorageUnavailable), "STORAGE_UNAVAILABLE"},
		{"digest mismatch and unknown fault", errors.Join(&domain.UploadVerificationChecksumMismatch{}, errors.New("fixture close error")), "INTERNAL_ERROR"},
		{"bare size mismatch without evidence", domain.ErrUploadBodySizeMismatch, "INTERNAL_ERROR"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if code := uploadVerificationErrorCode(scenario.err); code != scenario.want {
				t.Fatalf("错误码为 %q，期望 %q", code, scenario.want)
			}
		})
	}
}
