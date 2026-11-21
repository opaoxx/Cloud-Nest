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

func TestObserveUnboundMultipartUploadCancellationSession(t *testing.T) {
	const key, etag, version = "fixture/unbound +%.bin", "fixture-etag", "Version+A/="
	const flow = "target head target"
	busy := domain.ErrUploadTaskBusy
	missing := domain.ErrUploadObjectMissing
	unavailable := domain.ErrUploadStorageUnavailable
	for _, fixture := range []struct {
		name, calls, cancelAt string
		firstError, lastError error
		statError, want       error
		bindAt                int
		withObject, changeKey bool
		wantObservation       bool
		changeTask            func(*domain.UploadTask)
	}{
		{name: "object missing", calls: flow, statError: fmt.Errorf("fixture HEAD: %w", missing), wantObservation: true},
		{name: "existing version", calls: flow, withObject: true, wantObservation: true},
		{name: "storage unavailable", calls: "target head", statError: unavailable, want: unavailable},
		{name: "storage timeout", calls: "target head", statError: context.DeadlineExceeded, want: context.DeadlineExceeded},
		{name: "empty success", calls: "target head", want: unavailable},
		{name: "contradictory missing", calls: "target head", withObject: true, statError: missing, want: unavailable},
		{name: "initial stale lease", calls: "target", firstError: busy, want: busy},
		{name: "initial binding changed", calls: "target", bindAt: 1, want: busy},
		{name: "lease lost after HEAD", calls: flow, withObject: true, lastError: busy, want: busy},
		{name: "binding changed after HEAD", calls: flow, statError: missing, bindAt: 2, want: busy},
		{name: "key changed after HEAD", calls: flow, statError: missing, changeKey: true, want: busy},
		{name: "canceled before lookup", cancelAt: "before", want: context.Canceled},
		{name: "canceled during HEAD", calls: "target head", statError: missing, cancelAt: "head", want: context.Canceled},
		{name: "canceled during recheck", calls: flow, withObject: true, cancelAt: "final", want: context.Canceled},
		{name: "already bound", changeTask: func(task *domain.UploadTask) { task.StorageUploadID = new("fixture-session") }, want: domain.ErrUploadTaskNotReady},
		{name: "wrong phase", changeTask: func(task *domain.UploadTask) { task.Status = domain.UploadStatusCreating }, want: domain.ErrInvalidUploadTaskData},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			task := &domain.UploadTask{ID: 19, UserID: 7, StagedObjectID: 23, Status: domain.UploadStatusCancelling,
				UploadMode: "MULTIPART", LeaseOwner: new("fixture-worker"), LeaseVersion: 5,
				LeaseUntil: new(time.Now().UTC().Add(time.Minute)), FileSize: 16777217}
			if fixture.changeTask != nil {
				fixture.changeTask(task)
			}
			if fixture.cancelAt == "before" {
				cancel()
			}
			// 1. 模拟任务和存储查询。未绑定分支的中止与会话复查入口必须保持未调用。
			var calls []string
			var lookups int
			object := &domain.UploadObjectResult{SizeBytes: 29, StorageETag: new(etag), StorageVersionID: new(version)}
			execution := NewMultipartUploadCancellationStorageService(
				func(callContext context.Context, claimed *domain.UploadTask) (*domain.MultipartUploadCancellationTarget, error) {
					calls = append(calls, "target")
					lookups++
					if callContext != ctx || !reflect.DeepEqual(claimed, task) {
						t.Fatal("原租约或上下文未传到查询")
					}
					if lookups == 1 && fixture.firstError != nil {
						return nil, fixture.firstError
					}
					if lookups == 2 && fixture.lastError != nil {
						return nil, fixture.lastError
					}
					target := &domain.MultipartUploadCancellationTarget{ObjectKey: key}
					if lookups == fixture.bindAt {
						target.UploadID = new("fixture-late-session")
					}
					if lookups == 2 && fixture.changeKey {
						target.ObjectKey = "fixture/new-key"
					}
					if lookups == 2 && fixture.cancelAt == "final" {
						cancel()
					}
					return target, nil
				},
				func(context.Context, string, string) error {
					t.Fatal("未绑定分支发起了中止")
					return nil
				},
				func(context.Context, string, string) (*domain.MultipartUploadAbortObservation, error) {
					t.Fatal("未绑定分支访问了分片会话")
					return nil, nil
				},
				func(callContext context.Context, objectKey string) (*domain.UploadObjectResult, error) {
					calls = append(calls, "head")
					if callContext != ctx || objectKey != key {
						t.Fatal("HEAD 未读取原对象键")
					}
					if fixture.cancelAt == "head" {
						cancel()
					}
					if fixture.withObject {
						return object, fixture.statError
					}
					return nil, fixture.statError
				})

			// 2. 核对查询次数与观察种类，再修改返回值确认它有独立副本。
			observed, err := execution.ObserveUnboundSession(ctx, task)
			if got := strings.Join(calls, " "); got != fixture.calls || !errors.Is(err, fixture.want) ||
				(observed != nil) != fixture.wantObservation {
				t.Fatal("只读查询顺序、观察或错误不符", got, observed, err)
			}
			if observed == nil {
				return
			}
			if observed.ObjectKey != key || !reflect.DeepEqual(observed.Task, *task) || (observed.Object != nil) != fixture.withObject {
				t.Fatal("观察未保留原键、空绑定、租约或对象状态")
			}
			*observed.Task.LeaseOwner = "changed lease copy"
			if *task.LeaseOwner != "fixture-worker" {
				t.Fatal("返回的租约与原任务共享可空字段")
			}
			if fixture.withObject {
				if !reflect.DeepEqual(observed.Object, object) {
					t.Fatal("对象元数据未保留")
				}
				*observed.Object.StorageETag, *observed.Object.StorageVersionID = "changed etag", "changed version"
				if *object.StorageETag != etag || *object.StorageVersionID != version {
					t.Fatal("返回的元数据与存储观察共享可空字段")
				}
			}
		})
	}
}
