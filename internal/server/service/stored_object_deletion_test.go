package service

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestStoredObjectDeletionProcessor(t *testing.T) {
	deleting := &domain.StoredObject{ID: 29, OwnerUserID: 7, Status: domain.StoredObjectStatusDeleting}
	completed := *deleting
	completed.Status = domain.StoredObjectStatusDeleted
	wrongObject := completed
	wrongObject.ID++
	writeFailure := errors.New("fixture writeback failure")

	for _, fixture := range []struct {
		name                    string
		removeError, writeError error
		cancelAfterRemoval      bool
		result                  *domain.StoredObject
		wantError               error
		wantCalls               []string
	}{
		{name: "storage confirms removal", result: &completed, wantCalls: []string{"remove", "complete"}},
		{name: "storage fails", removeError: domain.ErrStoredObjectDeleteUnavailable, wantError: domain.ErrStoredObjectDeleteUnavailable,
			wantCalls: []string{"remove"}},
		{name: "cancel after removal", cancelAfterRemoval: true, wantError: context.Canceled, wantCalls: []string{"remove"}},
		{name: "writeback fails", writeError: writeFailure, wantError: writeFailure, wantCalls: []string{"remove", "complete"}},
		{name: "writeback has no result", wantError: domain.ErrInvalidStoredObjectData, wantCalls: []string{"remove", "complete"}},
		{name: "writeback returns another object", result: &wrongObject, wantError: domain.ErrInvalidStoredObjectData,
			wantCalls: []string{"remove", "complete"}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 记录两层调用，核对失败或取消后是否继续写回。
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var calls []string
			processor := NewStoredObjectDeletionProcessor(func(callContext context.Context, record *domain.StoredObject) error {
				calls = append(calls, "remove")
				if callContext != ctx || record != deleting {
					t.Fatal("存储删除收到的上下文或对象错误")
				}
				if fixture.cancelAfterRemoval {
					cancel()
				}
				return fixture.removeError
			}, func(callContext context.Context, record *domain.StoredObject) (*domain.StoredObject, error) {
				calls = append(calls, "complete")
				if callContext != ctx || record != deleting {
					t.Fatal("写回收到的上下文或对象错误")
				}
				return fixture.result, fixture.writeError
			})
			result, err := processor.Process(ctx, deleting)
			if !errors.Is(err, fixture.wantError) || !slices.Equal(calls, fixture.wantCalls) ||
				(err == nil && result != &completed) || (err != nil && result != nil) {
				t.Fatal("删除编排的结果或调用顺序不符", result, err, calls)
			}
		})
	}
}

func TestStoredObjectDeletionProcessorRejectsInvalidRecord(t *testing.T) {
	processor := NewStoredObjectDeletionProcessor(func(context.Context, *domain.StoredObject) error {
		t.Fatal("无效对象或已取消的请求进入了存储层")
		return nil
	}, func(context.Context, *domain.StoredObject) (*domain.StoredObject, error) {
		t.Fatal("无效对象或已取消的请求进入了写回")
		return nil, nil
	})
	ready := &domain.StoredObject{ID: 29, OwnerUserID: 7, Status: domain.StoredObjectStatusReady}
	for _, object := range []*domain.StoredObject{nil, ready} {
		if result, err := processor.Process(t.Context(), object); result != nil || !errors.Is(err, domain.ErrInvalidStoredObjectData) {
			t.Fatal("无效删除对象未被拒绝", result, err)
		}
	}
	ready.Status = domain.StoredObjectStatusDeleting
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if result, err := processor.Process(ctx, ready); result != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("已取消请求未保留原因", result, err)
	}
}
