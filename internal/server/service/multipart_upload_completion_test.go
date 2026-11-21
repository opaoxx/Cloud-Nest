package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestMultipartUploadCompletionServiceComplete(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 123456000, time.UTC)
	actor := domain.Actor{UserID: 7, DeviceID: 31, SessionID: 41}
	claims := []domain.UploadPartCompletionClaim{{PartNumber: 2, ETag: "Tail"}, {PartNumber: 1, ETag: "First"}}
	receipts := []domain.UploadPartReceipt{uploadPartReceiptFixture(1, receiptFixturePartSize, "First"), uploadPartReceiptFixture(2, 1, "Tail")}
	readFault := &domain.UploadPartDeclarationError{Field: "parts.0.etag", Message: "ETag 无效"}
	missingParts := &domain.UploadPartDeclarationError{Field: "parts", Message: "完成请求须提交分片回执"}
	commitFault := errors.New("fixture commit failure")
	releaseFault := errors.New("fixture release failure")
	mismatch := &domain.UploadPartsMismatchError{PartNumbers: []uint32{1}}
	for _, test := range []struct {
		name                                          string
		status, wantStatus                            domain.UploadStatus
		findError, readError, claimError, verifyError error
		commitError, releaseError, wantError          error
		noReader, emptyBody, staleRelease             bool
		cancelAt                                      string
		wantCalls                                     []string
	}{
		{name: "success", wantStatus: domain.UploadStatusCompleting, wantCalls: []string{"find", "read", "claim", "verify", "commit"}},
		{name: "replay completing", status: domain.UploadStatusCompleting, wantStatus: domain.UploadStatusCompleting, wantCalls: []string{"find"}},
		{name: "replay verifying", status: domain.UploadStatusVerifying, wantStatus: domain.UploadStatusVerifying, wantCalls: []string{"find"}},
		{name: "replay completed without body", status: domain.UploadStatusCompleted, noReader: true,
			wantStatus: domain.UploadStatusCompleted, wantCalls: []string{"find"}},
		{name: "task not found", findError: domain.ErrUploadTaskNotFound, wantError: domain.ErrUploadTaskNotFound, wantCalls: []string{"find"}},
		{name: "missing reader", noReader: true, wantError: missingParts, wantCalls: []string{"find"}},
		{name: "empty body", emptyBody: true, wantError: missingParts, wantCalls: []string{"find", "read"}},
		{name: "read error", readError: readFault, wantError: readFault, wantCalls: []string{"find", "read"}},
		{name: "claim busy", claimError: domain.ErrUploadTaskBusy, wantError: domain.ErrUploadTaskBusy, wantCalls: []string{"find", "read", "claim"}},
		{name: "mismatch releases", verifyError: mismatch, wantError: mismatch, wantCalls: []string{"find", "read", "claim", "verify", "release"}},
		{name: "lease already replaced", verifyError: domain.ErrUploadTaskBusy, staleRelease: true,
			wantError: domain.ErrUploadTaskBusy, wantCalls: []string{"find", "read", "claim", "verify", "release"}},
		{name: "commit failure releases", commitError: commitFault, wantError: commitFault,
			wantCalls: []string{"find", "read", "claim", "verify", "commit", "release"}},
		{name: "release failure keeps primary error", verifyError: mismatch, releaseError: releaseFault, wantError: mismatch,
			wantCalls: []string{"find", "read", "claim", "verify", "release"}},
		{name: "cancel right after claim", cancelAt: "claim", wantError: context.Canceled, wantCalls: []string{"find", "read", "claim", "release"}},
		{name: "cancel during verify", cancelAt: "verify", wantError: context.Canceled, wantCalls: []string{"find", "read", "claim", "verify", "release"}},
		{name: "cancel after successful commit", cancelAt: "commit", wantStatus: domain.UploadStatusCompleting,
			wantCalls: []string{"find", "read", "claim", "verify", "commit"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			type contextKey struct{}
			ctx, cancel := context.WithCancel(context.WithValue(t.Context(), contextKey{}, "fixture-request"))
			defer cancel()
			task := uploadPartSigningTaskFixture(now.Add(time.Hour))
			if test.status != "" {
				task.Status = test.status
				task.ExpiresAt = now.Add(-time.Minute)
			}
			if task.Status == domain.UploadStatusCompleted {
				task.ReservedBytes = 0
				task.PublishedObjectID, task.ResultEntryID, task.CompletedAt = new(uint64(25)), new(uint64(31)), &now
			}
			// 1. 绑定编排依赖，记录每种结果的实际执行顺序。
			var calls []string
			var leased *domain.UploadTask
			assertLease := func(callContext context.Context, currentActor domain.Actor, claimed *domain.UploadTask) {
				t.Helper()
				if callContext != ctx || currentActor != actor || claimed != leased {
					t.Fatal("编排没有使用原请求、认证身份和已提交租约")
				}
			}
			app := NewMultipartUploadCompletionService(
				func(callContext context.Context, userID, taskID uint64) (*domain.UploadTask, time.Time, error) {
					calls = append(calls, "find")
					if callContext != ctx || userID != actor.UserID || taskID != task.ID {
						t.Fatal("读取未使用可信账号和完整任务 ID")
					}
					return task, now, test.findError
				},
				func(callContext context.Context, currentActor domain.Actor, expected *domain.UploadTask, owner string,
					resolve func(*domain.UploadTask, uint64, uint64, time.Time) (domain.UploadStatus, error)) (*domain.UploadTask, error) {
					calls = append(calls, "claim")
					_, ownerError := uuid.Parse(owner)
					status, stageError := resolve(expected, actor.UserID, task.ID, now)
					if callContext != ctx || currentActor != actor || expected == task || !reflect.DeepEqual(expected, task) ||
						ownerError != nil || status != domain.UploadStatusUploading || stageError != nil {
						t.Fatal("领取没有收到独立快照、有效持有者和阶段规则")
					}
					if test.claimError != nil {
						return nil, test.claimError
					}
					leased = copyUploadTask(expected)
					leased.LeaseVersion++
					leased.LeaseOwner, leased.LeaseUntil = &owner, new(now.Add(time.Minute))
					if test.cancelAt == "claim" {
						cancel()
					}
					return leased, nil
				},
				func(callContext context.Context, currentActor domain.Actor, claimed *domain.UploadTask, declared []domain.UploadPartCompletionClaim) ([]domain.UploadPartReceipt, error) {
					calls = append(calls, "verify")
					assertLease(callContext, currentActor, claimed)
					if !reflect.DeepEqual(declared, claims) {
						t.Fatal("核对未承接客户端回执")
					}
					if test.cancelAt == "verify" {
						cancel()
					}
					return receipts, test.verifyError
				},
				func(callContext context.Context, currentActor domain.Actor, claimed *domain.UploadTask, matched []domain.UploadPartReceipt) (*domain.UploadTask, error) {
					calls = append(calls, "commit")
					assertLease(callContext, currentActor, claimed)
					if !reflect.DeepEqual(matched, receipts) {
						t.Fatal("提交未使用经过核对的存储回执")
					}
					committed := copyUploadTask(claimed)
					committed.Status, committed.LeaseOwner, committed.LeaseUntil = domain.UploadStatusCompleting, nil, nil
					if test.cancelAt == "commit" {
						cancel()
					}
					return committed, test.commitError
				},
				func(callContext context.Context, currentActor domain.Actor, claimed *domain.UploadTask) (bool, error) {
					calls = append(calls, "release")
					deadline, hasDeadline := callContext.Deadline()
					if currentActor != actor || claimed != leased || callContext.Err() != nil || callContext.Value(contextKey{}) != "fixture-request" ||
						!hasDeadline || time.Until(deadline) > 5*time.Second {
						t.Fatal("清理未使用本次租约和独立的有界上下文")
					}
					return !test.staleRelease, test.releaseError
				},
			)
			readClaims := func() ([]domain.UploadPartCompletionClaim, error) {
				calls = append(calls, "read")
				if test.emptyBody {
					return nil, nil
				}
				return claims, test.readError
			}
			if test.noReader {
				readClaims = nil
			}

			// 2. 对照步骤、返回结果和失败清理行为。
			result, err := app.Complete(ctx, actor, task.ID, readClaims)
			if !reflect.DeepEqual(calls, test.wantCalls) {
				t.Fatal("调用顺序错误", calls, test.wantCalls)
			}
			if test.wantError != nil {
				wantDeclaration, declarationExpected := test.wantError.(*domain.UploadPartDeclarationError)
				if declarationExpected {
					var declaration *domain.UploadPartDeclarationError
					if !errors.As(err, &declaration) || declaration.Field != wantDeclaration.Field {
						t.Fatal("声明错误未保留字段", err)
					}
				} else if !errors.Is(err, test.wantError) {
					t.Fatal("原始错误没有保留", err)
				}
				if result != nil || (test.releaseError != nil && !errors.Is(err, test.releaseError)) {
					t.Fatal("失败错误或交付边界错误", result, err)
				}
				return
			}
			if err != nil || result == nil || result == task || result.Status != test.wantStatus ||
				!reflect.DeepEqual(result.ResultEntryID, task.ResultEntryID) {
				t.Fatal("成功结果没有保留已提交阶段或发布结果", result, err)
			}
		})
	}
}
