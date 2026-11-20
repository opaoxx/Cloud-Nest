package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestIdempotentPurgeEntryServiceIntegration(t *testing.T) {
	// 1. 使用正式表准备回收项，接入签发器、用户事务和 DTO。
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	pool, store, actor := trashEntryIdempotencyTestDatabase(t, ctx)
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='TRASHED', deleted_at='2000-01-01 00:00:00.000000' WHERE user_id=1 AND id=13"); err != nil {
		t.Fatal(err)
	}
	masterKey := make([]byte, sha256.Size)
	signer, err := service.NewPurgePreviewSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewPurgePreviewVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	issuedAt := time.Now().UTC().Truncate(time.Microsecond)
	now := issuedAt
	previewService := service.NewPurgePreviewService(func(ctx context.Context, action func(context.Context, service.PurgePreviewReader) error) error {
		return store.WithFileEntrySnapshot(ctx, func(readContext context.Context, readStore *Store) error {
			return action(readContext, readStore)
		})
	}, signer, func() time.Time { return issuedAt })
	withTransaction := func(ctx context.Context, identity domain.Actor, action func(context.Context, service.IdempotentPurgeEntryTransaction) error) error {
		return store.WithUserTransaction(ctx, identity, func(txContext context.Context, txStore *Store) error {
			return action(txContext, txStore)
		})
	}
	encode := func(entry *domain.FileEntry) (json.RawMessage, error) {
		data, err := dto.NewPurgeEntryAcceptedData(entry)
		if err != nil {
			return nil, err
		}
		return json.Marshal(data)
	}
	decodeEntryID := func(body []byte) (string, error) {
		data, err := dto.DecodePurgeEntryAcceptedData(body)
		if err != nil {
			return "", err
		}
		return data.EntryID, nil
	}
	currentTime := func() time.Time { return now }
	purge := service.NewIdempotentPurgeEntryService(withTransaction, verifier, encode, decodeEntryID, currentTime)
	preview := func(t *testing.T, entryID uint64) *domain.PurgePreview {
		t.Helper()
		result, err := previewService.Preview(ctx, actor, entryID)
		if err != nil || result == nil {
			t.Fatal("未取得预检结果", err)
		}
		return result
	}
	readEntry := func(t *testing.T, entryID uint64) *domain.FileEntry {
		t.Helper()
		entry, err := store.FindUserFileEntry(ctx, actor.UserID, entryID)
		if err != nil {
			t.Fatal(err)
		}
		return entry
	}

	// 2. 核对首次提交和历史快照复用。
	t.Run("first acceptance and replay after token expiry and state change", func(t *testing.T) {
		const key = "purge-first"
		request := preview(t, 13)
		first, err := purge.Begin(ctx, actor, request.EntryID, request.Revision, request.Credential.PreviewDigest, key)
		if err != nil {
			t.Fatal(err)
		}
		data, err := dto.DecodePurgeEntryAcceptedData(first)
		if err != nil || data == nil || data.EntryID != "13" || data.Status != "PURGING" ||
			readEntry(t, 13).Revision != request.Revision+1 || readEntry(t, 13).Status != "PURGING" {
			t.Fatal("首次受理状态和返回快照不符", data, err)
		}
		record, err := store.FindUserIdempotencyRecord(ctx, actor.UserID, domain.PurgeEntryOperation, key)
		hash, hashError := service.BuildPurgeEntryRequestHash(request.EntryID, request.Revision, request.Credential.PreviewDigest)
		if err != nil || hashError != nil || record == nil || !bytes.Equal(first, record.ResponseBody) ||
			record.RequestHash != hash || record.ResponseStatus != 202 || record.ResourceID == nil || *record.ResourceID != 13 ||
			!record.ExpiresAt.Equal(issuedAt.Add(8*24*time.Hour)) {
			t.Fatal("已提交的 202 记录不符", err, hashError)
		}

		// 模拟后续清理已完成，原记录仍可在自己的保留期内交付。
		if _, err := pool.ExecContext(ctx, `UPDATE file_entries SET status='PURGED', object_id=NULL,
			purged_at=UTC_TIMESTAMP(6), revision=revision+1 WHERE user_id=1 AND id=13`); err != nil {
			t.Fatal(err)
		}
		now = request.Credential.ExpiresAt.Add(time.Second)
		defer func() { now = issuedAt }()
		finished := readEntry(t, 13)
		replay, err := purge.Begin(ctx, actor, request.EntryID, request.Revision, request.Credential.PreviewDigest, key)
		if err != nil || !bytes.Equal(first, replay) || !reflect.DeepEqual(readEntry(t, 13), finished) {
			t.Fatal("重试未复用首次 202 快照", err)
		}
		clear(replay)
		if body, err := purge.Begin(ctx, actor, request.EntryID, request.Revision+1, request.Credential.PreviewDigest, key); body != nil ||
			!errors.Is(err, domain.ErrIdempotencyRequestConflict) {
			t.Fatal("同键异参未产生冲突", err)
		}
		if body, err := purge.Begin(ctx, actor, request.EntryID, request.Revision, request.Credential.PreviewDigest, "purge-new-key"); body != nil ||
			!errors.Is(err, domain.ErrFileEntryNotFound) {
			t.Fatal("新键应按首次受理核对当前清理状态", err)
		}
		now = record.ExpiresAt
		if body, err := purge.Begin(ctx, actor, request.EntryID, request.Revision, request.Credential.PreviewDigest, key); body != nil ||
			!errors.Is(err, domain.ErrIdempotencyRecordExpired) {
			t.Fatal("记录到期后应停止复用", err)
		}
	})

	// 3. 模拟快照已写入但事务失败，两处写入都应回滚。
	t.Run("rollback of state and receipt", func(t *testing.T) {
		const key = "purge-rollback"
		request := preview(t, 14)
		before := readEntry(t, 14)
		failure := errors.New("受理快照已写入后的事务故障")
		failingPurge := service.NewIdempotentPurgeEntryService(func(ctx context.Context, identity domain.Actor, action func(context.Context, service.IdempotentPurgeEntryTransaction) error) error {
			return store.WithUserTransaction(ctx, identity, func(txContext context.Context, txStore *Store) error {
				return action(txContext, restoreSaveFailureStore{Store: txStore, failure: failure})
			})
		}, verifier, encode, decodeEntryID, currentTime)
		body, err := failingPurge.Begin(ctx, actor, request.EntryID, request.Revision, request.Credential.PreviewDigest, key)
		var startedAt sql.NullTime
		readError := pool.QueryRowContext(ctx, "SELECT purge_started_at FROM file_entries WHERE user_id=1 AND id=14").Scan(&startedAt)
		record, recordError := store.FindUserIdempotencyRecord(ctx, actor.UserID, domain.PurgeEntryOperation, key)
		if body != nil || !errors.Is(err, failure) || readError != nil || startedAt.Valid ||
			!reflect.DeepEqual(readEntry(t, 14), before) || record != nil || !errors.Is(recordError, domain.ErrIdempotencyRecordNotFound) {
			t.Fatal("回滚后状态或受理记录未恢复", err, readError, recordError)
		}
	})

	// 4. 相同幂等键的并发请求共享一份快照。
	t.Run("concurrent retries", func(t *testing.T) {
		const key = "purge-concurrent"
		request := preview(t, 14)
		pool.SetMaxOpenConns(2)
		defer pool.SetMaxOpenConns(1)
		type result struct {
			body json.RawMessage
			err  error
		}
		start := make(chan struct{})
		results := make(chan result, 2)
		for range 2 {
			go func() {
				<-start
				body, err := purge.Begin(ctx, actor, request.EntryID, request.Revision, request.Credential.PreviewDigest, key)
				results <- result{body: body, err: err}
			}()
		}
		close(start)
		first, second := <-results, <-results
		entry := readEntry(t, 14)
		if first.err != nil || second.err != nil || len(first.body) == 0 || !bytes.Equal(first.body, second.body) ||
			entry.Status != "PURGING" || entry.Revision != request.Revision+1 {
			t.Fatal("同键并发应只写入一次并交付同一快照", first.err, second.err, entry.Revision)
		}
	})
}
