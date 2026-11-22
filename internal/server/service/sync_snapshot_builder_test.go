package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

type syncSnapshotReadFunc func(context.Context) ([]domain.SyncSnapshotEntry, error)

func (read syncSnapshotReadFunc) ListSyncSnapshotEntries(ctx context.Context) ([]domain.SyncSnapshotEntry, error) {
	return read(ctx)
}

func TestSyncSnapshotBuilderBuild(t *testing.T) {
	// 1. 注入读取错误、事务结束错误和取消，记录各步骤实际发生的顺序。
	readFailure := errors.New("fixture read failed")
	endFailure := errors.New("fixture transaction failed to end")
	baseActor := domain.Actor{UserID: 7, DeviceID: 12, SessionID: 15}
	header := domain.SyncSnapshotHeader{RootEntryID: 11, HighWaterSequence: 0}
	for _, testCase := range []struct {
		name      string
		readError error
		endError  error
		cancelAt  string
		wantError error
		wantSteps []string
	}{
		{name: "success after closing transaction", wantSteps: []string{"open", "read", "end", "encode"}},
		{name: "read failure", readError: readFailure, wantError: readFailure, wantSteps: []string{"open", "read"}},
		{name: "transaction end failure", endError: endFailure, wantError: endFailure, wantSteps: []string{"open", "read", "end"}},
		{name: "already canceled", cancelAt: "start", wantError: context.Canceled},
		{name: "cancel before encoding", cancelAt: "end", wantError: context.Canceled, wantSteps: []string{"open", "read", "end"}},
		{name: "cancel during encoding", cancelAt: "encode", wantError: context.Canceled, wantSteps: []string{"open", "read", "end", "encode"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if testCase.cancelAt == "start" {
				cancel()
			}
			var steps []string
			builder := NewSyncSnapshotBuilder(func(requestContext context.Context, userID uint64, action func(context.Context, SyncSnapshotReader, domain.SyncSnapshotHeader) error) error {
				steps = append(steps, "open")
				if userID != baseActor.UserID || requestContext != ctx {
					t.Fatal("事务使用了错误的当前身份或上下文")
				}
				readContext, release := context.WithCancel(requestContext)
				defer release()
				reader := syncSnapshotReadFunc(func(current context.Context) ([]domain.SyncSnapshotEntry, error) {
					steps = append(steps, "read")
					if current != readContext {
						t.Fatal("读取必须使用事务上下文")
					}
					return []domain.SyncSnapshotEntry{}, testCase.readError
				})
				if err := action(readContext, reader, header); err != nil {
					return err
				}
				steps = append(steps, "end")
				if testCase.cancelAt == "end" {
					cancel()
				}
				return testCase.endError
			}, func(entries []domain.SyncSnapshotEntry) (*domain.SyncSnapshotEntriesJSON, error) {
				steps = append(steps, "encode")
				if testCase.cancelAt == "encode" {
					cancel()
				}
				return dto.EncodeSyncSnapshotEntries(entries)
			})

			// 2. 核对错误不交付快照，成功时保留零截止序号和空数组。
			built, err := builder.Build(ctx, baseActor)
			if !errors.Is(err, testCase.wantError) || !reflect.DeepEqual(steps, testCase.wantSteps) {
				t.Fatalf("实际顺序=%v，预期=%v，错误=%v", steps, testCase.wantSteps, err)
			}
			if testCase.wantError != nil {
				if built != nil {
					t.Fatal("失败时交付了快照")
				}
				return
			}
			if built == nil || built.Header != header || built.Entries.Items == nil || len(built.Entries.Items) != 0 || built.Entries.SizeBytes != 2 {
				t.Fatalf("空树的构建结果错误，结果=%+v", built)
			}
		})
	}
}

func TestSyncSnapshotBuilderRejectsOversizedEncoding(t *testing.T) {
	// 1. 条目数合法，使用可转义名称让真实 JSON 编码超过 8 MiB。
	entries := make([]domain.SyncSnapshotEntry, domain.MaxSyncSnapshotEntries)
	digest := sha256.Sum256(nil)
	name := strings.Repeat("&", 120)
	for index := range entries {
		entries[index] = domain.SyncSnapshotEntry{
			ID: math.MaxUint64 - uint64(len(entries)-index), ParentID: math.MaxUint64,
			Name: name, EntryType: "FILE", Revision: math.MaxUint64, VerifiedSHA256: &digest,
		}
	}
	builder := NewSyncSnapshotBuilder(func(ctx context.Context, userID uint64, action func(context.Context, SyncSnapshotReader, domain.SyncSnapshotHeader) error) error {
		return action(ctx, syncSnapshotReadFunc(func(context.Context) ([]domain.SyncSnapshotEntry, error) {
			return entries, nil
		}), domain.SyncSnapshotHeader{RootEntryID: math.MaxUint64, HighWaterSequence: 3})
	}, dto.EncodeSyncSnapshotEntries)

	// 2. 构建层保留容量错误，头部和部分条目都不会返回。
	identity := domain.Actor{UserID: 7, DeviceID: 12, SessionID: 15}
	if built, err := builder.Build(t.Context(), identity); built != nil || !errors.Is(err, domain.ErrSyncSnapshotTooLarge) {
		t.Fatalf("超限时应拒绝整个快照，错误=%v", err)
	}
}
