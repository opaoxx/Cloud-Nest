package service

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestObserveBoundMultipartUploadCancellationSession(t *testing.T) {
	const key, session = "fixture/original +%.bin", "Opaque+A/= "
	const version, etag = "Version+A/=", "observed-etag"
	const fullFlow = "target abort check target"
	unknown := domain.ErrUploadStorageUnavailable
	busy := domain.ErrUploadTaskBusy
	for _, fixture := range []struct {
		name, calls, cancelAt        string
		firstError, recheckError     error
		abortError, checkError, want error
		noTarget, changeKey          bool
		changeSessionAt              int
		pending, withObject          bool
		wantObservation              bool
		changeTask                   func(*domain.UploadTask)
	}{
		{name: "confirmed empty", calls: fullFlow, wantObservation: true},
		{name: "explicit missing still checks", calls: fullFlow, abortError: fmt.Errorf("fixture abort: %w", domain.ErrUploadPartSessionMissing), wantObservation: true},
		{name: "completed version and observed size", calls: fullFlow, withObject: true, wantObservation: true},
		{name: "pending still rechecks lease", calls: fullFlow, pending: true},
		{name: "unknown abort stops this attempt", calls: "target abort", abortError: unknown, want: unknown},
		{name: "transport timeout stops this attempt", calls: "target abort", abortError: context.DeadlineExceeded, want: context.DeadlineExceeded},
		{name: "storage check timeout", calls: "target abort check", checkError: context.DeadlineExceeded, want: context.DeadlineExceeded},
		{name: "target lookup timeout", calls: "target", firstError: context.DeadlineExceeded, want: context.DeadlineExceeded},
		{name: "readback failure", calls: "target abort check", checkError: unknown, want: unknown},
		{name: "stale initial lease", calls: "target", firstError: busy, want: busy},
		{name: "missing target", calls: "target", noTarget: true, want: domain.ErrInvalidStoredObjectData},
		{name: "initial raw binding mismatch", calls: "target", changeSessionAt: 1, want: busy},
		{name: "stale final lease discards object", calls: fullFlow, withObject: true, recheckError: busy, want: busy},
		{name: "pending target also lost lease", calls: fullFlow, pending: true, recheckError: busy, want: busy},
		{name: "object key changed", calls: fullFlow, changeKey: true, want: busy},
		{name: "raw session changed", calls: fullFlow, changeSessionAt: 2, want: busy},
		{name: "canceled before starting", cancelAt: "before", want: context.Canceled},
		{name: "canceled during target lookup", calls: "target", cancelAt: "target", want: context.Canceled},
		{name: "canceled during abort", calls: "target abort", cancelAt: "abort", want: context.Canceled},
		{name: "canceled during storage check", calls: "target abort check", cancelAt: "check", want: context.Canceled},
		{name: "canceled during final lookup", calls: fullFlow, cancelAt: "final", want: context.Canceled},
		{name: "unbound session", changeTask: func(task *domain.UploadTask) { task.StorageUploadID = nil }, want: domain.ErrUploadTaskNotReady},
		{name: "invalid session", changeTask: func(task *domain.UploadTask) { task.StorageUploadID = new(" ") }, want: domain.ErrInvalidUploadTaskData},
		{name: "other phase", changeTask: func(task *domain.UploadTask) { task.Status = domain.UploadStatusUploading }, want: domain.ErrInvalidUploadTaskData},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			task := &domain.UploadTask{ID: 19, UserID: 7, StagedObjectID: 23, Status: domain.UploadStatusCancelling,
				UploadMode: "MULTIPART", StorageUploadID: new(session), LeaseOwner: new("fixture-worker"), LeaseVersion: 5,
				LeaseUntil: new(time.Now().UTC().Add(time.Minute)), FileSize: 16777217}
			if fixture.changeTask != nil {
				fixture.changeTask(task)
			}
			if fixture.cancelAt == "before" {
				cancel()
			}
			// 1. 记录每次调用，模拟存储和仓储在不同阶段返回的结果。
			var calls []string
			var lookups int
			stored := &domain.UploadObjectResult{SizeBytes: 29, StorageETag: new(etag), StorageVersionID: new(version)}
			execution := NewMultipartUploadCancellationStorageService(
				func(callContext context.Context, claimed *domain.UploadTask) (*domain.MultipartUploadCancellationTarget, error) {
					calls = append(calls, "target")
					lookups++
					if callContext != ctx || !reflect.DeepEqual(claimed, task) {
						t.Fatal("目标查询没有使用原租约或上下文")
					}
					if (lookups == 1 && fixture.cancelAt == "target") || (lookups == 2 && fixture.cancelAt == "final") {
						cancel()
					}
					if lookups == 1 && fixture.firstError != nil {
						return nil, fixture.firstError
					}
					if lookups == 2 && fixture.recheckError != nil {
						return nil, fixture.recheckError
					}
					if fixture.noTarget {
						return nil, nil
					}
					target := &domain.MultipartUploadCancellationTarget{ObjectKey: key, UploadID: new(session)}
					if lookups == 2 && fixture.changeKey {
						target.ObjectKey = "fixture/replacement"
					}
					if lookups == fixture.changeSessionAt {
						target.UploadID = new(strings.TrimSpace(session))
					}
					return target, nil
				},
				func(callContext context.Context, objectKey, uploadID string) error {
					calls = append(calls, "abort")
					if callContext != ctx || objectKey != key || uploadID != session {
						t.Fatal("中止未保留原始对象键或会话 ID")
					}
					if fixture.cancelAt == "abort" {
						cancel()
					}
					return fixture.abortError
				},
				func(callContext context.Context, objectKey, uploadID string) (*domain.MultipartUploadAbortObservation, error) {
					calls = append(calls, "check")
					if callContext != ctx || objectKey != key || uploadID != session {
						t.Fatal("复查没有沿用本轮目标")
					}
					if fixture.cancelAt == "check" {
						cancel()
					}
					if fixture.checkError != nil || fixture.pending {
						return nil, fixture.checkError
					}
					checked := &domain.MultipartUploadAbortObservation{}
					if fixture.withObject {
						checked.Object = stored
					}
					return checked, nil
				},
				func(context.Context, string) (*domain.UploadObjectResult, error) {
					t.Fatal("已绑定分支应通过会话复查读取对象")
					return nil, nil
				})
			// 2. 核对调用顺序、中止次数及证据。粘连的可空字段不能影响原记录。
			observed, err := execution.ObserveBoundSession(ctx, task)
			if got := strings.Join(calls, " "); got != fixture.calls || !errors.Is(err, fixture.want) ||
				(observed != nil) != fixture.wantObservation {
				t.Fatal("取消观察、调用顺序或错误不符", got, observed, err)
			}
			if errors.Is(err, context.DeadlineExceeded) && errors.Is(err, domain.ErrUploadStorageUnavailable) != (fixture.firstError == nil) {
				t.Fatal("未区分存储操作和目标查询的超时", err)
			}
			if observed == nil {
				return
			}
			if observed.ObjectKey != key || !reflect.DeepEqual(observed.Task, *task) || (observed.Object != nil) != fixture.withObject {
				t.Fatal("观察未保留原任务、对象定位或正确结果种类")
			}
			*observed.Task.StorageUploadID = "changed result"
			if *task.StorageUploadID != session {
				t.Fatal("返回的任务快照影响了原租约")
			}
			if fixture.withObject {
				if !reflect.DeepEqual(observed.Object, stored) {
					t.Fatal("未保留存储报告的实际大小、版本和 ETag")
				}
				*observed.Object.StorageETag, *observed.Object.StorageVersionID = "changed etag", "changed version"
				if *stored.StorageETag != etag || *stored.StorageVersionID != version {
					t.Fatal("返回的观察与存储结果共享引用")
				}
			}
		})
	}
}
