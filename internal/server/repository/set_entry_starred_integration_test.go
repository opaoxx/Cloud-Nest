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
	"strconv"
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

// starredResultStore 在真实收藏写入后注入异常，验证核心服务的结果核对及回滚。
type starredResultStore struct {
	*Store
	afterSet func(*domain.EntryPreference)
}

func (store *starredResultStore) SetUserEntryStarred(ctx context.Context, entryID uint64, starred bool) (*domain.EntryPreference, error) {
	preference, err := store.Store.SetUserEntryStarred(ctx, entryID, starred)
	if err == nil && store.afterSet != nil {
		store.afterSet(preference)
	}
	return preference, err
}

func TestSetUserEntryStarredIntegration(t *testing.T) {
	// 1. 正式表独占库准备当前身份、文件/目录及不同偏好状态。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(3)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, boundary := range []struct{ table, next string }{{"auth_sessions", "stored_objects"}, {"user_entry_preferences", "upload_tasks"}} {
		start := strings.Index(migrations.InitialSQL, "CREATE TABLE "+boundary.table+" (")
		end := strings.Index(migrations.InitialSQL, "CREATE TABLE "+boundary.next+" (")
		if start < 0 || end <= start {
			t.Fatal("正式表边界错误")
		}
		if _, err := pool.ExecContext(ctx, migrations.InitialSQL[start:end]); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,status,storage_quota,used_storage) VALUES(1,'star_one','收藏用户','fixture_hash','ACTIVE',4096,2048),(3,'star_other','其他用户','fixture_hash','ACTIVE',4096,0)",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(19,1,'28ee758a-0727-42de-ae7d-3bb33155e148','收藏设备','WEB')",
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,1,19,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','star-entry',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR'),(12,1,11,'父目录',CAST('父目录' AS BINARY),'DIR'),(14,1,11,'无偏好目录',CAST('无偏好目录' AS BINARY),'DIR'),(15,1,11,'仅访问目录',CAST('仅访问目录' AS BINARY),'DIR'),(16,1,11,'空偏好目录',CAST('空偏好目录' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision) VALUES(13,1,12,'收藏笔记.md',CAST('收藏笔记.md' AS BINARY),'FILE','DOCUMENT',41,2048,3)",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	oldStamp := time.Date(2020, 1, 2, 3, 4, 5, 123456000, time.UTC)
	if _, err := pool.ExecContext(ctx, "INSERT INTO user_entry_preferences(user_id,entry_id,starred_at,last_opened_at,created_at,updated_at) VALUES(1,15,NULL,?,?,?),(1,16,NULL,NULL,?,?)", oldStamp, oldStamp, oldStamp, oldStamp, oldStamp); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: 1, DeviceID: 19, SessionID: 17}
	starredService := service.NewSetStarredService(func(ctx context.Context, actor domain.Actor, action func(context.Context, service.SetStarredTransaction) error) error {
		return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			return action(txContext, txStore)
		})
	})
	set := func(callContext context.Context, entryID uint64, starred bool) (*domain.EntryPreference, error) {
		return starredService.Set(callContext, actor, entryID, starred)
	}
	read := func(entryID uint64) *domain.EntryPreference {
		t.Helper()
		preference, err := store.FindUserEntryPreference(ctx, 1, entryID)
		if err != nil {
			t.Fatal(err)
		}
		return preference
	}
	audit := func(entryID uint64) [2]time.Time {
		t.Helper()
		var stamps [2]time.Time
		if err := pool.QueryRowContext(ctx, "SELECT created_at,updated_at FROM user_entry_preferences WHERE user_id=1 AND entry_id=?", entryID).Scan(&stamps[0], &stamps[1]); err != nil {
			t.Fatal(err)
		}
		return stamps
	}
	fileBefore, err := store.FindUserFileEntry(ctx, 1, 13)
	if err != nil {
		t.Fatal(err)
	}

	// 2. 首次、重复及取消收藏核对业务和审计时间。
	t.Run("single connection first repeat and unstar", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		first, err := set(ctx, 13, true)
		if err != nil || first == nil || first.StarredAt == nil || first.LastOpenedAt != nil || first.StarredAt.Location() != time.UTC || first.StarredAt.Nanosecond()%1000 != 0 {
			t.Fatal("首次收藏事实错误", err)
		}
		firstStamp := *first.StarredAt
		beforeAudit := audit(13)
		*first.StarredAt = oldStamp
		repeated, err := set(ctx, 13, true)
		if err != nil || repeated == nil || repeated.StarredAt == nil || !repeated.StarredAt.Equal(firstStamp) || audit(13) != beforeAudit {
			t.Fatal("重复收藏事实错误", err)
		}
		unstarred, err := set(ctx, 13, false)
		if err != nil || unstarred == nil || unstarred.StarredAt != nil || unstarred.LastOpenedAt != nil {
			t.Fatal("取消收藏事实错误", err)
		}
		unstarAudit := audit(13)
		if unstarAudit[0] != beforeAudit[0] {
			t.Fatal("创建时间错误")
		}
		if _, err := set(ctx, 13, false); err != nil || audit(13) != unstarAudit {
			t.Fatal("重复取消收藏的审计事实错误", err)
		}
		fileAfter, err := store.FindUserFileEntry(ctx, 1, 13)
		if err != nil || !reflect.DeepEqual(fileBefore, fileAfter) {
			t.Fatal("文件基础事实与收藏操作混用", err)
		}
	})
	t.Run("missing preference directory root and recent access", func(t *testing.T) {
		missing, err := set(ctx, 14, false)
		var count int
		if err != nil || missing == nil || missing.StarredAt != nil || missing.LastOpenedAt != nil {
			t.Fatal("缺失偏好取消收藏错误", err)
		}
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM user_entry_preferences WHERE user_id=1 AND entry_id=14").Scan(&count); err != nil || count != 0 {
			t.Fatal("缺失偏好取消收藏的行数错误", err)
		}
		for _, entryID := range []uint64{11, 15, 16} {
			preference, err := set(ctx, entryID, true)
			if err != nil || preference == nil || preference.StarredAt == nil {
				t.Fatal("目录或根收藏错误", err)
			}
			if entryID == 15 && (preference.LastOpenedAt == nil || !preference.LastOpenedAt.Equal(oldStamp)) {
				t.Fatal("最近访问时间错误")
			}
			if entryID == 16 && preference.LastOpenedAt != nil {
				t.Fatal("空访问事实错误")
			}
		}
		before := audit(15)
		if !before[0].Equal(oldStamp) || !before[1].After(oldStamp) {
			t.Fatal("已有偏好收藏后的审计时间错误")
		}
		repeated, err := set(ctx, 15, true)
		if err != nil || repeated.LastOpenedAt == nil || !repeated.LastOpenedAt.Equal(oldStamp) || audit(15) != before {
			t.Fatal("重复收藏与访问事实错误", err)
		}
		unstarred, err := set(ctx, 15, false)
		if err != nil || unstarred.StarredAt != nil || unstarred.LastOpenedAt == nil || !unstarred.LastOpenedAt.Equal(oldStamp) {
			t.Fatal("取消收藏后的访问事实错误", err)
		}
	})
	t.Run("scope and hidden target or ancestor", func(t *testing.T) {
		for _, entryID := range []uint64{33, 999} {
			if result, err := set(ctx, entryID, true); result != nil || !errors.Is(err, domain.ErrFileEntryNotFound) {
				t.Fatal("收藏范围检查错误", err)
			}
		}
		before := read(13)
		for _, entryID := range []uint64{12, 13} {
			for _, status := range []string{"TRASHED", "PURGING", "PURGED"} {
				if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status=?,deleted_at=UTC_TIMESTAMP(6),purge_started_at=IF(?='TRASHED',NULL,UTC_TIMESTAMP(6)),purged_at=IF(?='PURGED',UTC_TIMESTAMP(6),NULL),object_id=IF(entry_type='FILE' AND ?='PURGED',NULL,object_id) WHERE id=?", status, status, status, status, entryID); err != nil {
					t.Fatal(err)
				}
				result, operationError := set(ctx, 13, true)
				if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='ACTIVE',deleted_at=NULL,purge_started_at=NULL,purged_at=NULL,object_id=IF(entry_type='FILE',41,NULL) WHERE id=?", entryID); err != nil {
					t.Fatal(err)
				}
				if result != nil || !errors.Is(operationError, domain.ErrFileEntryNotFound) || !reflect.DeepEqual(before, read(13)) {
					t.Fatal("隐藏路径收藏错误", operationError)
				}
			}
		}
	})
	// 3. 已提交偏好转换为公开响应，核对状态、时间、空值和独立结果。
	t.Run("response DTO from committed preferences", func(t *testing.T) {
		for _, entryID := range []uint64{13, 15, 11, 14} {
			baseBefore, err := store.FindUserFileEntry(ctx, actor.UserID, entryID)
			if err != nil {
				t.Fatal(err)
			}
			var snapshot *dto.SetStarredData
			for _, starred := range []bool{false, true, true, false} {
				preference, err := set(ctx, entryID, starred)
				if err != nil {
					t.Fatal(err)
				}
				data, err := dto.NewSetStarredData(preference)
				if err != nil || data == nil || data.Starred != starred || (data.StarredAt != nil) != starred || !reflect.DeepEqual(preference, read(entryID)) {
					t.Fatal("已提交收藏偏好转换错误", err)
				}
				if starred {
					if data.StarredAt == preference.StarredAt || !data.StarredAt.Equal(*preference.StarredAt) || data.StarredAt.Location() != time.UTC || data.StarredAt.Nanosecond()%1000 != 0 {
						t.Fatal("收藏时间事实或独立结果错误")
					}
					if snapshot != nil && !snapshot.StarredAt.Equal(*data.StarredAt) {
						t.Fatal("重复收藏的响应时间错误")
					}
					snapshot = data
					*preference.StarredAt = oldStamp
					if !data.StarredAt.Equal(*read(entryID).StarredAt) {
						t.Fatal("来源偏好修改影响响应时间")
					}
				}
				encoded, err := json.Marshal(dto.SetStarredResponse{Data: *data, RequestID: "28ee758a-0727-42de-ae7d-3bb33155e148"})
				if err != nil {
					t.Fatal(err)
				}
				var body map[string]json.RawMessage
				if err := json.Unmarshal(encoded, &body); err != nil || len(body) != 2 || string(body["request_id"]) != `"28ee758a-0727-42de-ae7d-3bb33155e148"` {
					t.Fatal("收藏响应外层字段错误", err)
				}
				var payload map[string]json.RawMessage
				if err := json.Unmarshal(body["data"], &payload); err != nil || len(payload) != 2 || string(payload["starred"]) != strconv.FormatBool(starred) {
					t.Fatal("收藏响应公开字段错误", err)
				}
				if !starred && string(payload["starred_at"]) != "null" {
					t.Fatal("取消收藏缺少显式 null")
				}
			}
			baseAfter, err := store.FindUserFileEntry(ctx, actor.UserID, entryID)
			if err != nil || !reflect.DeepEqual(baseBefore, baseAfter) || snapshot == nil || !snapshot.Starred || snapshot.StarredAt == nil {
				t.Fatal("收藏响应快照或目录项基础事实错误", err)
			}
			if entryID == 15 && !read(entryID).LastOpenedAt.Equal(oldStamp) {
				t.Fatal("响应转换影响最近访问时间")
			}
		}
	})
	// 4. 实际写入后外层失败或取消，统一回滚偏好。
	t.Run("outer failure and cancellation rollback", func(t *testing.T) {
		failure := errors.New("fixture starred rollback")
		for _, cancelAfterWrite := range []bool{false, true} {
			before, beforeAudit := read(13), audit(13)
			callContext, cancelCall := context.WithCancel(ctx)
			err := store.WithUserTransaction(callContext, actor, func(txContext context.Context, txStore *Store) error {
				if _, err := txStore.SetUserEntryStarred(txContext, 13, true); err != nil {
					return err
				}
				if cancelAfterWrite {
					cancelCall()
					return txContext.Err()
				}
				return failure
			})
			cancelCall()
			want := failure
			if cancelAfterWrite {
				want = context.Canceled
			}
			if !errors.Is(err, want) || !reflect.DeepEqual(before, read(13)) || audit(13) != beforeAudit {
				t.Fatal("收藏写后回滚错误", err)
			}
		}
	})
	t.Run("service invalid result and cancellation rollback", func(t *testing.T) {
		for _, mode := range []string{"wrong owner", "wrong target", "wrong state", "zero time", "cancel after write", "outer callback failure"} {
			t.Run(mode, func(t *testing.T) {
				before, beforeAudit := read(13), audit(13)
				callContext, cancelCall := context.WithCancel(ctx)
				defer cancelCall()
				failure := errors.New("fixture outer callback failure")
				injected := service.NewSetStarredService(func(ctx context.Context, actor domain.Actor, action func(context.Context, service.SetStarredTransaction) error) error {
					return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
						err := action(txContext, &starredResultStore{Store: txStore, afterSet: func(preference *domain.EntryPreference) {
							switch mode {
							case "wrong owner":
								preference.UserID++
							case "wrong target":
								preference.EntryID++
							case "wrong state":
								preference.StarredAt = nil
							case "zero time":
								*preference.StarredAt = time.Time{}
							case "cancel after write":
								cancelCall()
							}
						}})
						if err == nil && mode == "outer callback failure" {
							return failure
						}
						return err
					})
				})
				result, err := injected.Set(callContext, actor, 13, true)
				want := domain.ErrInvalidFileEntryData
				if mode == "cancel after write" {
					want = context.Canceled
				}
				if mode == "outer callback failure" {
					want = failure
				}
				if result != nil || !errors.Is(err, want) || !reflect.DeepEqual(read(13), before) || audit(13) != beforeAudit {
					t.Fatal("服务写后失败未完整回滚偏好及审计事实", err)
				}
			})
		}
	})
	t.Run("service confirmed commit then canceled", func(t *testing.T) {
		callContext, cancelCall := context.WithCancel(ctx)
		defer cancelCall()
		committed := service.NewSetStarredService(func(ctx context.Context, actor domain.Actor, action func(context.Context, service.SetStarredTransaction) error) error {
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				return action(txContext, txStore)
			})
			if err == nil {
				cancelCall()
			}
			return err
		})
		result, err := committed.Set(callContext, actor, 13, true)
		if err != nil || result == nil || callContext.Err() != context.Canceled || !reflect.DeepEqual(result, read(13)) {
			t.Fatal("真实提交后的收藏事实错误", err)
		}
		if _, err := set(ctx, 13, false); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("identity recheck", func(t *testing.T) {
		for _, item := range []struct {
			change, restore string
			want            error
		}{
			{"UPDATE users SET status='DISABLED' WHERE id=1", "UPDATE users SET status='ACTIVE' WHERE id=1", domain.ErrAccountDisabled},
			{"UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=19", "UPDATE user_devices SET revoked_at=NULL WHERE id=19", domain.ErrDeviceRevoked},
			{"UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=17", "UPDATE auth_sessions SET revoked_at=NULL WHERE id=17", domain.ErrSessionRevoked},
		} {
			before := read(13)
			if _, err := pool.ExecContext(ctx, item.change); err != nil {
				t.Fatal(err)
			}
			result, operationError := set(ctx, 13, true)
			if _, err := pool.ExecContext(ctx, item.restore); err != nil {
				t.Fatal(err)
			}
			if result != nil || !errors.Is(operationError, item.want) || !reflect.DeepEqual(before, read(13)) {
				t.Fatal("收藏身份复查错误", operationError)
			}
		}
	})
	t.Run("current preference over old snapshot", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET starred_at=NULL,last_opened_at=? WHERE user_id=1 AND entry_id=13", oldStamp); err != nil {
			t.Fatal(err)
		}
		newStar, newOpened := oldStamp.Add(time.Hour), oldStamp.Add(2*time.Hour)
		var current *domain.EntryPreference
		err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			previous, err := txStore.FindUserEntryPreference(txContext, 1, 13)
			if err != nil || previous.StarredAt != nil {
				return errors.New("旧快照准备错误")
			}
			// 独立连接的受控夹具更新用于验证当前读。
			if _, err := pool.ExecContext(txContext, "UPDATE user_entry_preferences SET starred_at=?,last_opened_at=?,updated_at=UTC_TIMESTAMP(6) WHERE user_id=1 AND entry_id=13", newStar, newOpened); err != nil {
				return err
			}
			current, err = txStore.SetUserEntryStarred(txContext, 13, true)
			return err
		})
		if err != nil || current == nil || current.StarredAt == nil || !current.StarredAt.Equal(newStar) || current.LastOpenedAt == nil || !current.LastOpenedAt.Equal(newOpened) {
			t.Fatal("收藏返回了旧快照事实", err)
		}
	})
	// 5. 用户锁协调并发状态设置，单条偏好记录保存实际结果。
	t.Run("concurrent identical and opposite states", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET starred_at=NULL WHERE user_id=1 AND entry_id=13"); err != nil {
			t.Fatal(err)
		}
		for _, states := range [][2]bool{{true, true}, {true, false}} {
			type outcome struct {
				preference *domain.EntryPreference
				err        error
				state      bool
			}
			results := make(chan outcome, 2)
			start := make(chan struct{})
			for _, state := range states {
				go func(state bool) {
					<-start
					preference, err := set(ctx, 13, state)
					results <- outcome{preference, err, state}
				}(state)
			}
			close(start)
			first, second := <-results, <-results
			for _, result := range []outcome{first, second} {
				if result.err != nil || result.preference == nil || (result.preference.StarredAt != nil) != result.state || result.preference.LastOpenedAt == nil || !result.preference.LastOpenedAt.Equal(oldStamp.Add(2*time.Hour)) {
					t.Fatal("并发收藏状态错误", result.err)
				}
			}
			if states[0] && states[1] && !first.preference.StarredAt.Equal(*second.preference.StarredAt) {
				t.Fatal("重复并发收藏时间错误")
			}
			var count int
			if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM user_entry_preferences WHERE user_id=1 AND entry_id=13").Scan(&count); err != nil || count != 1 {
				t.Fatal("并发偏好数量错误", err)
			}
		}
	})
	// 6. 实际 JWT、认证中间件和 PUT 处理函数连接真实收藏事务。
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
		access, err := signer.Issue(domain.LoginSession{ID: actor.SessionID, UserID: actor.UserID, DeviceID: actor.DeviceID, ExpiresAt: time.Now().Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		authorization := service.NewAuthorizationService(verifier.Verify, store.FindSessionAuthorization)
		var disableAfterAuthorization atomic.Bool
		router := serverhttp.NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
		router.PUT("/test-set-starred/:id", middleware.RequireAuthorization(func(ctx context.Context, token string) (*domain.Actor, error) {
			current, err := authorization.Authorize(ctx, token)
			if err == nil && disableAfterAuthorization.Swap(false) {
				_, err = pool.ExecContext(ctx, "UPDATE users SET status='DISABLED' WHERE id=1")
			}
			return current, err
		}, handler.AuthorizationFailure), handler.SetStarred(starredService.Set))
		server := httptest.NewServer(router)
		defer server.Close()
		client := server.Client()
		client.Timeout = 3 * time.Second
		requestNumber := 0
		request := func(id, body, token string, status int, code, field string) dto.SetStarredResponse {
			t.Helper()
			requestNumber++
			requestID := fmt.Sprintf("28ee758a-0727-42de-ae7d-%012d", requestNumber)
			message, err := http.NewRequestWithContext(ctx, "PUT", server.URL+"/test-set-starred/"+id+"?note=private-query", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			message.Header.Set("Content-Type", "application/json")
			message.Header.Set("X-Request-ID", requestID)
			if token != "" {
				message.Header.Set("Authorization", "Bearer "+token)
			}
			response, err := client.Do(message)
			if err != nil {
				t.Fatal("真实收藏 HTTP 失败", err)
			}
			encoded, readError := io.ReadAll(response.Body)
			closeError := response.Body.Close()
			if readError != nil || closeError != nil || response.StatusCode != status || response.Header.Get("X-Request-ID") != requestID || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Content-Type") != "application/json; charset=utf-8" {
				t.Fatal("真实收藏响应状态或头信息错误", response.StatusCode, readError, closeError)
			}
			var result dto.SetStarredResponse
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
			if field == "" {
				if failed.Error.Details != nil {
					t.Fatal("真实错误详情须为 null")
				}
			} else if len(failed.Error.Details) != 1 || failed.Error.Details["field"] != field {
				t.Fatal("真实字段错误位置不符")
			}
			return result
		}
		first := request("13", `{"starred":true}`, access.Token, 200, "", "")
		repeated := request("13", `{"starred":true}`, access.Token, 200, "", "")
		if !first.Data.Starred || first.Data.StarredAt == nil || !reflect.DeepEqual(first.Data, repeated.Data) || first.RequestID == repeated.RequestID || !first.Data.StarredAt.Equal(*read(13).StarredAt) {
			t.Fatal("真实收藏重试的状态、时间或请求 ID 错误")
		}
		unstarred := request("13", `{"starred":false}`, access.Token, 200, "", "")
		if unstarred.Data.Starred || unstarred.Data.StarredAt != nil || read(13).StarredAt != nil {
			t.Fatal("真实取消收藏错误")
		}
		for _, id := range []string{"15", "11"} {
			response := request(id, `{"starred":true}`, access.Token, 200, "", "")
			if !response.Data.Starred || response.Data.StarredAt == nil {
				t.Fatal("真实目录或根目录收藏错误")
			}
		}
		request("33", `{"starred":true}`, access.Token, 404, "ENTRY_NOT_FOUND", "")
		request("999", `{"starred":true}`, access.Token, 404, "ENTRY_NOT_FOUND", "")
		before := read(13)
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id=12"); err != nil {
			t.Fatal(err)
		}
		request("13", `{"starred":true}`, access.Token, 404, "ENTRY_NOT_FOUND", "")
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='ACTIVE',deleted_at=NULL WHERE id=12"); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, read(13)) {
			t.Fatal("祖先不可见时收藏偏好发生改变")
		}
		request("0", `{"private-broken":`, access.Token, 400, "INVALID_ARGUMENT", "id")
		request("13", `{}`, access.Token, 400, "INVALID_ARGUMENT", "starred")
		request("13", `{"starred":null}`, access.Token, 400, "INVALID_ARGUMENT", "starred")
		request("0", `{"private-broken":`, "", 401, "AUTH_REQUIRED", "")
		request("13", `{"starred":true}`, "invalid-access", 401, "AUTH_REQUIRED", "")
		disableAfterAuthorization.Store(true)
		request("13", `{"starred":true}`, access.Token, 403, "ACCOUNT_DISABLED", "")
		if _, err := pool.ExecContext(ctx, "UPDATE users SET status='ACTIVE' WHERE id=1"); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, read(13)) || !read(15).LastOpenedAt.Equal(oldStamp) {
			t.Fatal("认证复查或请求处理改变了原有偏好事实")
		}
	})
	t.Run("maximum user target revision and date", func(t *testing.T) {
		maximum := ^uint64(0)
		for _, statement := range []struct {
			sql  string
			args []any
		}{
			{"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(?,'max_starred','最大收藏用户','fixture_hash','ACTIVE')", []any{maximum}},
			{"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,revision) VALUES(?,?,NULL,'',X'','DIR',1),(?,?,?,'最大目录',CAST('最大目录' AS BINARY),'DIR',?)", []any{maximum - 1, maximum, maximum, maximum, maximum - 1, maximum}},
			{"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,'ca67141f-9801-40be-aa25-d7cb9cdc74e6','最大设备','WEB')", []any{maximum, maximum}},
			{"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(?,?,?,UNHEX(REPEAT('22',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)", []any{maximum, maximum, maximum}},
		} {
			if _, err := pool.ExecContext(ctx, statement.sql, statement.args...); err != nil {
				t.Fatal(err)
			}
		}
		maximumActor := domain.Actor{UserID: maximum, DeviceID: maximum, SessionID: maximum}
		var result *domain.EntryPreference
		setMaximum := func(starred bool) error {
			var err error
			result, err = starredService.Set(ctx, maximumActor, maximum, starred)
			return err
		}
		if err := setMaximum(true); err != nil || result == nil || result.UserID != maximum || result.EntryID != maximum || result.StarredAt == nil {
			t.Fatal("最大标识收藏错误", err)
		}
		data, err := dto.NewSetStarredData(result)
		if err != nil || data == nil || !data.Starred || data.StarredAt == nil || !data.StarredAt.Equal(*result.StarredAt) {
			t.Fatal("最大标识收藏响应转换错误", err)
		}
		maximumOpened := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
		if _, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET last_opened_at=? WHERE user_id=? AND entry_id=?", maximumOpened, maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if err := setMaximum(false); err != nil || result.StarredAt != nil || result.LastOpenedAt == nil || !result.LastOpenedAt.Equal(maximumOpened) {
			t.Fatal("最大日期及取消收藏错误", err)
		}
	})
}
