package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	serverhttp "github.com/opaoxx/cloud-nest/internal/server/http"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestIdempotentMoveEntryServiceIntegration(t *testing.T) {
	// 1. 使用正式表独占库，绑定实际用户事务和已有 DTO 编解码入口。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(3)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, boundary := range []struct{ table, next string }{{"auth_sessions", "stored_objects"}, {"user_entry_preferences", "upload_tasks"}, {"idempotency_records", ""}} {
		start := strings.Index(migrations.InitialSQL, "CREATE TABLE "+boundary.table+" (")
		end := len(migrations.InitialSQL)
		if boundary.next != "" {
			end = strings.Index(migrations.InitialSQL, "CREATE TABLE "+boundary.next+" (")
		}
		if start < 0 || end <= start {
			t.Fatal("正式表边界错误")
		}
		if _, err := pool.ExecContext(ctx, migrations.InitialSQL[start:end]); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(1,'move_service','幂等移动用户','fixture_hash','ACTIVE'),(3,'move_other','其他用户','fixture_hash','ACTIVE')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,color_key) VALUES(11,1,NULL,'',X'','DIR',NULL),(33,3,NULL,'',X'','DIR',NULL),(15,1,11,'资料',CAST('资料' AS BINARY),'DIR','green'),(17,1,15,'子目录',CAST('子目录' AS BINARY),'DIR',NULL),(16,1,11,'目的地',CAST('目的地' AS BINARY),'DIR',NULL),(18,1,11,'后续目的地',CAST('后续目的地' AS BINARY),'DIR',NULL),(19,1,11,'冲突目的地',CAST('冲突目的地' AS BINARY),'DIR',NULL),(20,1,19,'REPORT.md',CAST('report.md' AS BINARY),'DIR',NULL)",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(13,1,'28ee758a-0727-42de-ae7d-3bb33155e148','幂等移动设备','WEB')",
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,1,13,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','idempotent-move',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,extension,object_id,file_size,revision,mime_type,source_device_id,source_device_name,content_modified_at) VALUES(14,1,15,'report.md',CAST('report.md' AS BINARY),'FILE','DOCUMENT','md',41,2048,3,'text/markdown',13,'来源设备 🎮',UTC_TIMESTAMP(6)-INTERVAL 1 DAY)",
		"INSERT INTO user_entry_preferences(user_id,entry_id,starred_at,last_opened_at) VALUES(1,14,UTC_TIMESTAMP(6)-INTERVAL 1 HOUR,UTC_TIMESTAMP(6))",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: 1, DeviceID: 13, SessionID: 17}
	withTransaction := func(ctx context.Context, current domain.Actor, action func(context.Context, service.IdempotentMoveEntryTransaction) error) error {
		return store.WithUserTransaction(ctx, current, func(txContext context.Context, txStore *Store) error { return action(txContext, txStore) })
	}
	encode := func(detail *domain.EntryDetail) (json.RawMessage, error) {
		data, err := dto.NewMoveEntryData(detail)
		if err != nil {
			return nil, err
		}
		return json.Marshal(data)
	}
	validate := func(body json.RawMessage) error {
		_, err := dto.RestoreMoveEntryData(body)
		return err
	}
	mover := service.NewIdempotentMoveEntryService(withTransaction, encode, validate)
	read := func(id uint64) *domain.FileEntry {
		t.Helper()
		entry, err := store.FindUserFileEntry(ctx, 1, id)
		if err != nil {
			t.Fatal(err)
		}
		return entry
	}
	countRecords := func() int {
		t.Helper()
		var count int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM idempotency_records").Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	call := func(t *testing.T, id, revision, targetID uint64, key string) *dto.MoveEntryData {
		t.Helper()
		body, err := mover.Move(ctx, actor, id, revision, targetID, key)
		if err != nil {
			t.Fatal("幂等移动失败", err)
		}
		data, err := dto.RestoreMoveEntryData(body)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	var first *dto.MoveEntryData
	// 2. 首次、重试和后续移动使用同一稳定快照协议。
	t.Run("single connection move and replay", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		first = call(t, 14, 3, 16, "move-007")
		record, err := store.FindUserIdempotencyRecord(ctx, 1, domain.MoveEntryOperation, "move-007")
		if err != nil || record.ResourceID == nil || *record.ResourceID != 14 || record.ResponseStatus != 200 || record.ExpiresAt.Sub(record.CreatedAt) < 7*24*time.Hour {
			t.Fatal("保存结果与期限错误", err)
		}
		if first.Entry.ParentID == nil || *first.Entry.ParentID != "16" || first.Entry.Name != "report.md" || first.Entry.Revision != "4" || first.Entry.Extension == nil || *first.Entry.Extension != "md" || first.Entry.Kind != "document" || first.Entry.MIMEType == nil || *first.Entry.MIMEType != "text/markdown" || first.Entry.SourceDevice == nil || !first.Entry.Starred || first.Entry.LastOpenedAt == nil {
			t.Fatal("完整文件快照错误")
		}
		before := countRecords()
		if replay := call(t, 14, 3, 16, "move-007"); !reflect.DeepEqual(first, replay) || read(14).Revision != 4 || countRecords() != before {
			t.Fatal("重试重复移动或快照错误")
		}
		call(t, 14, 4, 18, "move-later")
		if _, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET starred_at=NULL,last_opened_at=NULL WHERE user_id=1 AND entry_id=14"); err != nil {
			t.Fatal(err)
		}
		if replay := call(t, 14, 3, 16, "move-007"); !reflect.DeepEqual(first, replay) || *read(14).ParentID != 18 || read(14).Revision != 5 {
			t.Fatal("原始快照混入后续状态")
		}
	})
	t.Run("folder and unchanged position", func(t *testing.T) {
		folder := call(t, 15, 1, 16, "move-folder")
		if folder.Entry.Kind != "folder" || folder.Entry.ParentID == nil || *folder.Entry.ParentID != "16" || folder.Entry.Extension != nil || folder.Entry.ColorKey == nil || *folder.Entry.ColorKey != "green" || *read(17).ParentID != 15 {
			t.Fatal("目录与后代快照错误")
		}
		before := read(14)
		data := call(t, 14, before.Revision, *before.ParentID, "move-same-position")
		if data.Entry.Revision != fmt.Sprint(before.Revision) || !reflect.DeepEqual(before, read(14)) {
			t.Fatal("同位置匹配发生了版本变化")
		}
	})
	t.Run("parameter conflict and fresh request errors", func(t *testing.T) {
		before, count := read(14), countRecords()
		for _, request := range []struct{ id, revision, target uint64 }{{15, 3, 16}, {14, 4, 16}, {14, 3, 18}} {
			if body, err := mover.Move(ctx, actor, request.id, request.revision, request.target, "move-007"); body != nil || !errors.Is(err, domain.ErrIdempotencyRequestConflict) {
				t.Fatal("同键参数冲突错误", err)
			}
		}
		for index, request := range []struct {
			id, revision, target uint64
			want                 error
		}{
			{14, 3, 16, domain.ErrEntryRevisionConflict}, {999, 1, 16, domain.ErrFileEntryNotFound},
			{33, 1, 16, domain.ErrFileEntryNotFound}, {11, 1, 16, domain.ErrRootEntryMove},
			{14, before.Revision, 33, domain.ErrFileEntryNotFound}, {14, before.Revision, 14, domain.ErrFileEntryNotDirectory},
			{15, 2, 15, domain.ErrEntryMoveIntoSubtree}, {15, 2, 17, domain.ErrEntryMoveIntoSubtree},
			{14, before.Revision, 19, domain.ErrEntryNameConflict},
		} {
			if body, err := mover.Move(ctx, actor, request.id, request.revision, request.target, fmt.Sprintf("fresh-%d", index)); body != nil || !errors.Is(err, request.want) {
				t.Fatal("新请求规则错误", err)
			}
		}
		if !reflect.DeepEqual(before, read(14)) || countRecords() != count {
			t.Fatal("失败请求产生了写入")
		}
	})

	// 3. 实际更新后注入失败，目录项与幂等结果共同回滚。
	t.Run("atomic rollback", func(t *testing.T) {
		failure := errors.New("fixture atomic move rollback")
		for _, mode := range []string{"encode", "save after insert", "outer callback", "cancel after encode", "validate after save"} {
			before, count := read(14), countRecords()
			beforeDisplay, err := store.FindUserFileEntryDisplay(ctx, 1, 14)
			if err != nil {
				t.Fatal(err)
			}
			writeContext, cancelWrite := context.WithCancel(ctx)
			encodeResult, validateResult, transaction := encode, validate, withTransaction
			switch mode {
			case "encode":
				encodeResult = func(*domain.EntryDetail) (json.RawMessage, error) { return nil, failure }
			case "save after insert", "outer callback":
				transaction = func(ctx context.Context, current domain.Actor, action func(context.Context, service.IdempotentMoveEntryTransaction) error) error {
					return store.WithUserTransaction(ctx, current, func(txContext context.Context, txStore *Store) error {
						if mode == "save after insert" {
							return action(txContext, &idempotentDirectorySaveFailureStore{Store: txStore, failure: failure})
						}
						if err := action(txContext, txStore); err != nil {
							return err
						}
						return failure
					})
				}
			case "cancel after encode":
				encodeResult = func(detail *domain.EntryDetail) (json.RawMessage, error) {
					body, err := encode(detail)
					cancelWrite()
					return body, err
				}
			case "validate after save":
				checks := 0
				validateResult = func(body json.RawMessage) error {
					checks++
					if checks == 2 {
						return failure
					}
					return validate(body)
				}
			}
			failing := service.NewIdempotentMoveEntryService(transaction, encodeResult, validateResult)
			body, err := failing.Move(writeContext, actor, 14, before.Revision, 16, "rollback-"+strings.ReplaceAll(mode, " ", "-"))
			cancelWrite()
			want := failure
			if mode == "cancel after encode" {
				want = context.Canceled
			}
			afterDisplay, readError := store.FindUserFileEntryDisplay(ctx, 1, 14)
			if body != nil || !errors.Is(err, want) || !reflect.DeepEqual(before, read(14)) || countRecords() != count || readError != nil || !reflect.DeepEqual(beforeDisplay, afterDisplay) {
				t.Fatal("移动和结果未完整回滚", mode, err, readError)
			}
		}
	})
	t.Run("identity recheck on replay", func(t *testing.T) {
		for _, mutation := range []struct {
			change, restore string
			want            error
		}{
			{"UPDATE users SET status='DISABLED' WHERE id=1", "UPDATE users SET status='ACTIVE' WHERE id=1", domain.ErrAccountDisabled},
			{"UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=13", "UPDATE user_devices SET revoked_at=NULL WHERE id=13", domain.ErrDeviceRevoked},
			{"UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=17", "UPDATE auth_sessions SET revoked_at=NULL WHERE id=17", domain.ErrSessionRevoked},
		} {
			if _, err := pool.ExecContext(ctx, mutation.change); err != nil {
				t.Fatal(err)
			}
			body, callError := mover.Move(ctx, actor, 14, 3, 16, "move-007")
			if _, err := pool.ExecContext(ctx, mutation.restore); err != nil {
				t.Fatal(err)
			}
			if body != nil || !errors.Is(callError, mutation.want) {
				t.Fatal("复用时的身份复查错误", callError)
			}
		}
	})

	// 4. 并发请求在用户锁内协调目的地、版本及记录唯一性。
	t.Run("concurrent keys parents and revisions", func(t *testing.T) {
		for _, mode := range []string{"same request", "different parents", "different keys"} {
			before, count := read(14), countRecords()
			targets := []uint64{}
			for _, target := range []uint64{15, 16, 18} {
				if target != *before.ParentID {
					targets = append(targets, target)
				}
			}
			type outcome struct {
				body json.RawMessage
				err  error
			}
			results := make(chan outcome, 2)
			start := make(chan struct{})
			for index := 0; index < 2; index++ {
				targetID, key := targets[0], "concurrent-"+strings.ReplaceAll(mode, " ", "-")
				if mode == "different parents" {
					targetID = targets[index]
				}
				if mode == "different keys" {
					key += fmt.Sprint(index)
				}
				go func() {
					<-start
					body, err := mover.Move(ctx, actor, 14, before.Revision, targetID, key)
					results <- outcome{body, err}
				}()
			}
			close(start)
			firstResult, secondResult := <-results, <-results
			successes, failures := 0, 0
			for _, result := range []outcome{firstResult, secondResult} {
				if result.err == nil {
					successes++
					continue
				}
				want := domain.ErrIdempotencyRequestConflict
				if mode == "different keys" {
					want = domain.ErrEntryRevisionConflict
				}
				if result.body != nil || !errors.Is(result.err, want) {
					t.Fatal("并发错误类型错误", mode, result.err)
				}
				failures++
			}
			if mode == "same request" {
				if successes != 2 || failures != 0 || !bytes.Equal(firstResult.body, secondResult.body) {
					t.Fatal("相同请求结果复用错误")
				}
			} else if successes != 1 || failures != 1 {
				t.Fatal("并发成功/失败数量错误")
			}
			if read(14).Revision != before.Revision+1 || countRecords() != count+1 {
				t.Fatal("并发重复移动或重复保存")
			}
		}
	})
	t.Run("expired and invalid saved snapshots", func(t *testing.T) {
		record, err := store.FindUserIdempotencyRecord(ctx, 1, domain.MoveEntryOperation, "move-007")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "UPDATE idempotency_records SET created_at=UTC_TIMESTAMP(6)-INTERVAL 9 DAY,expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 DAY WHERE id=?", record.ID); err != nil {
			t.Fatal(err)
		}
		body, callError := mover.Move(ctx, actor, 14, 3, 16, "move-007")
		if _, err := pool.ExecContext(ctx, "UPDATE idempotency_records SET created_at=?,expires_at=? WHERE id=?", record.CreatedAt, record.ExpiresAt, record.ID); err != nil {
			t.Fatal(err)
		}
		if body != nil || !errors.Is(callError, domain.ErrIdempotencyRecordExpired) {
			t.Fatal("到期结果未拒绝", callError)
		}
		for _, statement := range []string{
			"UPDATE idempotency_records SET response_body=JSON_SET(response_body,'$.entry.kind','UNKNOWN') WHERE id=?",
			"UPDATE idempotency_records SET response_body=JSON_SET(response_body,'$.entry.parent_id','11') WHERE id=?",
		} {
			if _, err := pool.ExecContext(ctx, statement, record.ID); err != nil {
				t.Fatal(err)
			}
			body, callError = mover.Move(ctx, actor, 14, 3, 16, "move-007")
			if _, err := pool.ExecContext(ctx, "UPDATE idempotency_records SET response_body=? WHERE id=?", string(record.ResponseBody), record.ID); err != nil {
				t.Fatal(err)
			}
			if body != nil || !errors.Is(callError, domain.ErrInvalidIdempotencyRecordData) {
				t.Fatal("异常快照未拒绝", callError)
			}
		}
	})
	// 5. 实际 JWT、认证中间件与 POST 处理函数连接现有用户事务。
	t.Run("authenticated HTTP handler", func(t *testing.T) {
		configuration := config.JWTConfig{SigningKey: bytes.Repeat([]byte{9}, 32), Issuer: "cloudnest-test", Audience: "cloudnest-api-test", AccessTokenTTL: 15 * time.Minute, RefreshTokenTTL: time.Hour}
		signer, err := service.NewAccessTokenSigner(configuration)
		if err != nil {
			t.Fatal(err)
		}
		verifier, err := service.NewAccessTokenVerifier(configuration)
		if err != nil {
			t.Fatal(err)
		}
		access, err := signer.Issue(domain.LoginSession{ID: 17, UserID: 1, DeviceID: 13, ExpiresAt: time.Now().Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		authorization := service.NewAuthorizationService(verifier.Verify, store.FindSessionAuthorization)
		var disableAfterAuthorization atomic.Bool
		router := serverhttp.NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
		router.POST("/test-move-entry/:id", middleware.RequireAuthorization(func(ctx context.Context, token string) (*domain.Actor, error) {
			current, err := authorization.Authorize(ctx, token)
			if err == nil && disableAfterAuthorization.Swap(false) {
				_, err = pool.ExecContext(ctx, "UPDATE users SET status='DISABLED' WHERE id=1")
			}
			return current, err
		}, handler.AuthorizationFailure), handler.MoveEntry(mover.Move))
		server := httptest.NewServer(router)
		defer server.Close()
		client := server.Client()
		client.Timeout = 3 * time.Second
		requestNumber := 0
		request := func(id, body, key, token string, status int, code string) dto.MoveEntryResponse {
			t.Helper()
			requestNumber++
			requestID := fmt.Sprintf("28ee758a-0727-42de-ae7d-%012d", requestNumber)
			message, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/test-move-entry/"+id+"?note=private-query", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			message.Header.Set("Content-Type", "application/json")
			message.Header.Set("X-Request-ID", requestID)
			if key != "" {
				message.Header.Set("Idempotency-Key", key)
			}
			if token != "" {
				message.Header.Set("Authorization", "Bearer "+token)
			}
			response, err := client.Do(message)
			if err != nil {
				t.Fatal("真实移动 HTTP 失败", err)
			}
			encoded, readError := io.ReadAll(response.Body)
			closeError := response.Body.Close()
			if readError != nil || closeError != nil || response.StatusCode != status || response.Header.Get("X-Request-ID") != requestID || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Content-Type") != "application/json; charset=utf-8" {
				t.Fatal("真实响应状态或头信息错误", response.StatusCode, readError, closeError)
			}
			var result dto.MoveEntryResponse
			if status == 200 {
				if json.Unmarshal(encoded, &result) != nil || result.RequestID != requestID {
					t.Fatal("真实成功响应错误")
				}
				return result
			}
			var failed dto.ErrorResponse
			if json.Unmarshal(encoded, &failed) != nil || failed.RequestID != requestID || failed.Error.Code != code {
				t.Fatal("真实错误响应错误", code)
			}
			if code == "REVISION_CONFLICT" && failed.Error.Details["current_revision"] != fmt.Sprint(read(14).Revision) {
				t.Fatal("真实当前版本错误")
			}
			return result
		}
		before := read(14)
		targetID := uint64(16)
		if *before.ParentID == targetID {
			targetID = 18
		}
		body := fmt.Sprintf(`{"target_parent_id":"%d","expected_revision":"%d"}`, targetID, before.Revision)
		firstResponse := request("14", body, "http-move-007", access.Token, 200, "")
		secondResponse := request("14", body, "http-move-007", access.Token, 200, "")
		if firstResponse.RequestID == secondResponse.RequestID || !reflect.DeepEqual(firstResponse.Data, secondResponse.Data) || read(14).Revision != before.Revision+1 || firstResponse.Data.Entry.ParentID == nil || *firstResponse.Data.Entry.ParentID != fmt.Sprint(targetID) {
			t.Fatal("真实重试结果、父目录或版本错误")
		}
		request("15", body, "http-move-007", access.Token, 409, "IDEMPOTENCY_CONFLICT")
		changedBody := fmt.Sprintf(`{"target_parent_id":"11","expected_revision":"%d"}`, before.Revision)
		request("14", changedBody, "http-move-007", access.Token, 409, "IDEMPOTENCY_CONFLICT")
		request("14", body, "http-stale-version", access.Token, 409, "REVISION_CONFLICT")
		request("11", `{"target_parent_id":"16","expected_revision":"1"}`, "http-root", access.Token, 403, "ROOT_OPERATION_FORBIDDEN")
		request("33", `{"target_parent_id":"16","expected_revision":"1"}`, "http-foreign-source", access.Token, 404, "ENTRY_NOT_FOUND")
		version := read(14).Revision
		request("14", fmt.Sprintf(`{"target_parent_id":"33","expected_revision":"%d"}`, version), "http-foreign-parent", access.Token, 404, "ENTRY_NOT_FOUND")
		request("14", fmt.Sprintf(`{"target_parent_id":"14","expected_revision":"%d"}`, version), "http-file-parent", access.Token, 400, "INVALID_ARGUMENT")
		request("15", `{"target_parent_id":"15","expected_revision":"2"}`, "http-self-parent", access.Token, 400, "INVALID_ARGUMENT")
		request("15", `{"target_parent_id":"17","expected_revision":"2"}`, "http-descendant", access.Token, 400, "INVALID_ARGUMENT")
		request("14", fmt.Sprintf(`{"target_parent_id":"19","expected_revision":"%d"}`, version), "http-name-conflict", access.Token, 409, "NAME_CONFLICT")
		request("14", `{"target_parent_id":"16","expected_revision":"0"}`, "http-invalid-version", access.Token, 400, "INVALID_ARGUMENT")
		request("0", `{"private-broken":`, "", "", 401, "AUTH_REQUIRED")
		disableAfterAuthorization.Store(true)
		request("14", body, "http-move-007", access.Token, 403, "ACCOUNT_DISABLED")
		if _, err := pool.ExecContext(ctx, "UPDATE users SET status='ACTIVE' WHERE id=1"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("maximum unsigned entry parent and revision", func(t *testing.T) {
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,revision) VALUES(?,1,11,'高位目的地',CAST('高位目的地' AS BINARY),'DIR',1),(?,1,11,'高位目录',CAST('高位目录' AS BINARY),'DIR',?)", maximum-1, maximum, maximum-1); err != nil {
			t.Fatal(err)
		}
		data := call(t, maximum, maximum-1, maximum-1, "maximum-entry")
		if data.Entry.ID != "18446744073709551615" || data.Entry.ParentID == nil || *data.Entry.ParentID != "18446744073709551614" || data.Entry.Revision != "18446744073709551615" || data.Entry.Kind != "folder" {
			t.Fatal("完整无符号快照错误")
		}
		if replay := call(t, maximum, maximum-1, maximum-1, "maximum-entry"); !reflect.DeepEqual(data, replay) {
			t.Fatal("完整无符号快照复用错误")
		}
		if same := call(t, maximum, maximum, maximum-1, "maximum-same-position"); !reflect.DeepEqual(data, same) {
			t.Fatal("最大版本同位置快照错误")
		}
	})
}
