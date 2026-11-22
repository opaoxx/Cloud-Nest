package repository

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	serverhttp "github.com/opaoxx/cloud-nest/internal/server/http"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestIdempotentTrashEntryServiceIntegration(t *testing.T) {
	// 1. 复用独占库及正式表，装配真实认证事务与 DTO 编解码。
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	pool, store, actor := trashEntryIdempotencyTestDatabase(t, ctx)
	installChangeEventFixtureTable(t, ctx, pool)
	withTransaction := func(ctx context.Context, actor domain.Actor, action func(context.Context, service.IdempotentTrashEntryTransaction) error) error {
		return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error { return action(txContext, txStore) })
	}
	encode := func(entry *domain.TrashedFileEntry) (json.RawMessage, error) {
		data, err := dto.NewTrashEntryData(entry)
		if err != nil {
			return nil, err
		}
		return json.Marshal(data)
	}
	validate := func(body json.RawMessage) error {
		_, err := dto.RestoreTrashEntryData(body)
		return err
	}
	trash := service.NewIdempotentTrashEntryService(withTransaction, encode, validate)
	read := func(t *testing.T, entryID uint64) *domain.FileEntry {
		t.Helper()
		entry, err := store.FindUserFileEntry(ctx, actor.UserID, entryID)
		if err != nil {
			t.Fatal(err)
		}
		return entry
	}
	readRecord := func(t *testing.T, key string) *domain.IdempotencyRecord {
		t.Helper()
		record, err := store.FindUserIdempotencyRecord(ctx, actor.UserID, domain.TrashEntryOperation, key)
		if err != nil {
			t.Fatal(err)
		}
		return record
	}
	readLog := func(t *testing.T) [2]uint64 {
		t.Helper()
		var log [2]uint64
		if err := pool.QueryRowContext(ctx, "SELECT change_seq,(SELECT COUNT(*) FROM change_events WHERE user_id=users.id) FROM users WHERE id=?",
			actor.UserID).Scan(&log[0], &log[1]); err != nil {
			t.Fatal(err)
		}
		return log
	}

	// 2. 通过实际 JWT 和测试 POST 串联请求处理、回收事务和快照交付。
	t.Run("first commit and historical replay", func(t *testing.T) {
		const key = "trash-first"
		const path = "/test-trash-entry/"
		configuration := config.JWTConfig{
			SigningKey: bytes.Repeat([]byte{9}, 32), Issuer: "cloudnest-test", Audience: "cloudnest-api-test",
			AccessTokenTTL: 15 * time.Minute, RefreshTokenTTL: time.Hour,
		}
		signer, err := service.NewAccessTokenSigner(configuration)
		if err != nil {
			t.Fatal(err)
		}
		verifier, err := service.NewAccessTokenVerifier(configuration)
		if err != nil {
			t.Fatal(err)
		}
		access, err := signer.Issue(domain.LoginSession{
			ID: actor.SessionID, UserID: actor.UserID, DeviceID: actor.DeviceID, ExpiresAt: time.Now().Add(time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		authorization := service.NewAuthorizationService(verifier.Verify, store.FindSessionAuthorization)
		router := serverhttp.NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
		router.POST(path+":id", middleware.RequireAuthorization(authorization.Authorize, handler.AuthorizationFailure), handler.TrashEntry(trash.Trash))
		server := httptest.NewServer(router)
		defer server.Close()
		client := server.Client()
		client.Timeout = 3 * time.Second
		send := func(entryID, revision uint64, key string, authenticated bool, status int, code string) dto.TrashEntryResponse {
			t.Helper()
			requestID := uuid.NewString()
			body := fmt.Sprintf(`{"expected_revision":"%d"}`, revision)
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s%s%d", server.URL, path, entryID), strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("X-Request-ID", requestID)
			request.Header.Set("Content-Type", "application/json")
			if key != "" {
				request.Header.Set("Idempotency-Key", key)
			}
			if authenticated {
				request.Header.Set("Authorization", "Bearer "+access.Token)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal("真实回收 HTTP 请求失败", err)
			}
			content, readError := io.ReadAll(response.Body)
			closeError := response.Body.Close()
			if readError != nil || closeError != nil || response.StatusCode != status ||
				response.Header.Get("X-Request-ID") != requestID || response.Header.Get("Cache-Control") != "no-store" ||
				(authenticated && response.Header.Get("Pragma") != "no-cache") {
				t.Fatal("真实回收响应状态或头错误", response.StatusCode, status, readError, closeError)
			}
			var delivered dto.TrashEntryResponse
			if status == http.StatusOK {
				if json.Unmarshal(content, &delivered) != nil || delivered.RequestID != requestID {
					t.Fatal("已提交的回收响应不符")
				}
				return delivered
			}
			var failure dto.ErrorResponse
			if json.Unmarshal(content, &failure) != nil || failure.RequestID != requestID || failure.Error.Code != code ||
				(code == "REVISION_CONFLICT" && failure.Error.Details["current_revision"] != "3") {
				t.Fatal("真实回收错误未按契约交付", failure.Error)
			}
			return delivered
		}
		send(0, 3, "", false, 401, "AUTH_REQUIRED")
		send(11, 1, key, true, 403, "ROOT_OPERATION_FORBIDDEN")
		send(21, 1, key, true, 404, "ENTRY_NOT_FOUND")
		send(13, 2, key, true, 409, "REVISION_CONFLICT")
		if readLog(t) != [2]uint64{} {
			t.Fatal("拒绝的请求推进了日志")
		}

		first := send(13, 3, key, true, 200, "")
		record := readRecord(t, key)
		snapshot, err := dto.RestoreTrashEntryData(record.ResponseBody)
		entry := read(t, 13)
		if err != nil || snapshot == nil || snapshot.Entry.ID != "13" || snapshot.Entry.Revision != "4" ||
			snapshot.Entry.Status != "TRASHED" || entry.Status != "TRASHED" || entry.Revision != 4 ||
			!entry.ModifiedAt.Equal(snapshot.Entry.DeletedAt) || !reflect.DeepEqual(&first.Data, snapshot) ||
			record.ExpiresAt.Sub(record.CreatedAt) < 7*24*time.Hour || readLog(t) != [2]uint64{1, 1} {
			t.Fatal("首次回收和稳定结果未一并提交", snapshot, err)
		}
		send(13, 4, key, true, 409, "IDEMPOTENCY_CONFLICT")
		send(13, 4, "trash-new-key", true, 404, "ENTRY_NOT_FOUND")

		// 直接布置后续状态，模拟该项已经从回收站恢复并改名。
		if _, err := pool.ExecContext(ctx, `UPDATE file_entries SET status='ACTIVE',deleted_at=NULL,revision=5,
			name='后来文件.md',name_key=CAST('后来文件.md' AS BINARY) WHERE user_id=1 AND id=13`); err != nil {
			t.Fatal(err)
		}
		before := read(t, 13)
		replay := send(13, 3, key, true, 200, "")
		if replay.RequestID == first.RequestID || !reflect.DeepEqual(replay.Data, first.Data) ||
			!reflect.DeepEqual(before, read(t, 13)) || readLog(t) != [2]uint64{1, 1} {
			t.Fatal("同键重试未保留原快照或改动了后续状态")
		}
		if _, err := pool.ExecContext(ctx, `UPDATE idempotency_records
			SET expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 DAY,created_at=UTC_TIMESTAMP(6)-INTERVAL 2 DAY
			WHERE user_id=? AND operation=? AND idempotency_key=?`, actor.UserID, domain.TrashEntryOperation, key); err != nil {
			t.Fatal(err)
		}
		send(13, 3, key, true, 409, "IDEMPOTENCY_EXPIRED")
	})

	// 3. 事务回调写入后返回失败时，状态、时间和幂等记录一起回滚。
	t.Run("outer failure after saving rolls back both writes", func(t *testing.T) {
		const key = "trash-rollback"
		before := read(t, 12)
		beforeLog := readLog(t)
		fixtureError := errors.New("fixture transaction fails after saving")
		failing := service.NewIdempotentTrashEntryService(
			func(ctx context.Context, actor domain.Actor, action func(context.Context, service.IdempotentTrashEntryTransaction) error) error {
				return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
					if err := action(txContext, txStore); err != nil {
						return err
					}
					return fixtureError
				})
			}, encode, validate,
		)
		body, err := failing.Trash(ctx, actor, 12, 1, key)
		record, readError := store.FindUserIdempotencyRecord(ctx, actor.UserID, domain.TrashEntryOperation, key)
		var deletedAt sql.NullTime
		timeError := pool.QueryRowContext(ctx, "SELECT deleted_at FROM file_entries WHERE user_id=1 AND id=12").Scan(&deletedAt)
		if body != nil || !errors.Is(err, fixtureError) || record != nil || !errors.Is(readError, domain.ErrIdempotencyRecordNotFound) ||
			timeError != nil || deletedAt.Valid || !reflect.DeepEqual(read(t, 12), before) || readLog(t) != beforeLog {
			t.Fatal("外层失败后未同时回滚目录项、幂等记录和变更日志", err, readError, timeError)
		}
	})

	// 4. 多连接并发同键同参，用户锁串行化首次回收。
	t.Run("concurrent same key returns one committed snapshot", func(t *testing.T) {
		const key = "trash-concurrent"
		pool.SetMaxOpenConns(3)
		defer pool.SetMaxOpenConns(1)
		type outcome struct {
			body json.RawMessage
			err  error
		}
		const callers = 4
		results := make(chan outcome, callers)
		start := make(chan struct{})
		for range callers {
			go func() {
				<-start
				body, err := trash.Trash(ctx, actor, 12, 1, key)
				results <- outcome{body: body, err: err}
			}()
		}
		close(start)
		observed := make([]outcome, 0, callers)
		for range callers {
			observed = append(observed, <-results)
		}
		committed := observed[0].body
		for _, result := range observed {
			if result.err != nil || len(result.body) == 0 || !bytes.Equal(result.body, committed) {
				t.Fatal("同键并发未返回相同快照", result.err)
			}
		}
		record := readRecord(t, key)
		entry := read(t, 12)
		if !bytes.Equal(record.ResponseBody, committed) || entry.Status != "TRASHED" || entry.Revision != 2 || readLog(t) != [2]uint64{2, 2} {
			t.Fatal("同键并发的保存快照或回收版本不符", entry)
		}
	})
}
