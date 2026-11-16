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

// recordAccessResultStore 在真实访问写入后注入异常，验证核心服务的校验和回滚。
type recordAccessResultStore struct {
	*Store
	afterRecord func(*domain.EntryPreference)
}

func (store *recordAccessResultStore) RecordUserEntryAccess(ctx context.Context, entryID uint64) (*domain.EntryPreference, error) {
	preference, err := store.Store.RecordUserEntryAccess(ctx, entryID)
	if err == nil && store.afterRecord != nil {
		store.afterRecord(preference)
	}
	return preference, err
}

func TestRecordUserEntryAccessIntegration(t *testing.T) {
	// 1. 正式表独占库准备身份、文件及不同偏好状态。
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
		"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(1,'access_one','访问用户','fixture_hash','ACTIVE'),(3,'access_other','其他用户','fixture_hash','ACTIVE')",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(19,1,'28ee758a-0727-42de-ae7d-3bb33155e148','访问设备','WEB')",
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,1,19,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','access-entry',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR'),(12,1,11,'访问父目录',CAST('访问父目录' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision) VALUES(13,1,12,'首次.md',CAST('首次.md' AS BINARY),'FILE','DOCUMENT',41,2048,3),(14,1,12,'已收藏.md',CAST('已收藏.md' AS BINARY),'FILE','DOCUMENT',41,2048,1),(15,1,12,'旧访问.md',CAST('旧访问.md' AS BINARY),'FILE','DOCUMENT',41,2048,1)",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Date(2020, 1, 2, 3, 4, 5, 123456000, time.UTC)
	if _, err := pool.ExecContext(ctx, "INSERT INTO user_entry_preferences(user_id,entry_id,starred_at,last_opened_at,created_at,updated_at) VALUES(1,14,?,NULL,?,?),(1,15,?,?,?,?)", old, old, old, old, old, old, old); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: 1, DeviceID: 19, SessionID: 17}
	accessService := service.NewRecordAccessService(func(ctx context.Context, actor domain.Actor, action func(context.Context, service.RecordAccessTransaction) error) error {
		return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			return action(txContext, txStore)
		})
	})
	record := func(callContext context.Context, entryID uint64) (*domain.EntryPreference, error) {
		return accessService.Record(callContext, actor, entryID, "preview")
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

	// 2. 单连接完成首次与窗口内访问，核对收藏、审计及基础事实。
	t.Run("single connection first and repeat", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		before, err := store.FindUserFileEntry(ctx, 1, 13)
		if err != nil {
			t.Fatal(err)
		}
		first, err := record(ctx, 13)
		if err != nil || first == nil || first.LastOpenedAt == nil || first.StarredAt != nil || first.LastOpenedAt.Location() != time.UTC || first.LastOpenedAt.Nanosecond()%1000 != 0 {
			t.Fatal("首次访问时间错误", err)
		}
		stamp, stamps := *first.LastOpenedAt, audit(13)
		*first.LastOpenedAt = old
		repeated, err := record(ctx, 13)
		if err != nil || repeated == nil || !repeated.LastOpenedAt.Equal(stamp) || audit(13) != stamps {
			t.Fatal("窗口内访问或审计时间错误", err)
		}
		after, err := store.FindUserFileEntry(ctx, 1, 13)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("访问与基础事实混用", err)
		}
	})
	t.Run("existing preference and star preservation", func(t *testing.T) {
		for _, entryID := range []uint64{14, 15} {
			before := audit(entryID)
			preference, err := record(ctx, entryID)
			if err != nil || preference == nil || preference.StarredAt == nil || !preference.StarredAt.Equal(old) || preference.LastOpenedAt == nil || !preference.LastOpenedAt.After(old) {
				t.Fatal("原收藏或新增访问时间错误", err)
			}
			after := audit(entryID)
			if before[0] != after[0] || !after[1].After(before[1]) {
				t.Fatal("偏好审计时间错误")
			}
			*preference.StarredAt, *preference.LastOpenedAt = old.Add(time.Hour), old
			current := read(entryID)
			if !current.StarredAt.Equal(old) || !current.LastOpenedAt.After(old) {
				t.Fatal("返回指针影响数据库偏好")
			}
		}
	})

	// 3. 服务提交后转换访问响应，对照收藏、访问及基础事实。
	t.Run("response DTO from committed accesses", func(t *testing.T) {
		for _, entryID := range []uint64{13, 14, 15} {
			baseBefore, err := store.FindUserFileEntry(ctx, actor.UserID, entryID)
			if err != nil {
				t.Fatal(err)
			}
			starBefore := read(entryID).StarredAt
			if _, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET last_opened_at=NULL,updated_at=? WHERE user_id=1 AND entry_id=?", old, entryID); err != nil {
				t.Fatal(err)
			}
			var previousData *dto.RecordAccessData
			for _, purpose := range []string{"preview", "open"} {
				preference, err := accessService.Record(ctx, actor, entryID, purpose)
				if err != nil {
					t.Fatal(err)
				}
				data, err := dto.NewRecordAccessData(preference)
				if err != nil || data == nil || !data.LastOpenedAt.Equal(*read(entryID).LastOpenedAt) || data.LastOpenedAt.Location() != time.UTC || data.LastOpenedAt.Nanosecond()%1000 != 0 {
					t.Fatal("已提交访问响应时间错误", err)
				}
				if previousData != nil && !reflect.DeepEqual(previousData, data) {
					t.Fatal("合并访问响应时间错误")
				}
				previousData = data
				*preference.LastOpenedAt = old
				if !data.LastOpenedAt.Equal(*read(entryID).LastOpenedAt) {
					t.Fatal("来源偏好修改影响访问响应")
				}
				encoded, err := json.Marshal(dto.RecordAccessResponse{Data: *data, RequestID: "28ee758a-0727-42de-ae7d-3bb33155e148"})
				if err != nil {
					t.Fatal(err)
				}
				var outer map[string]json.RawMessage
				var fields map[string]json.RawMessage
				if json.Unmarshal(encoded, &outer) != nil || len(outer) != 2 || string(outer["request_id"]) != `"28ee758a-0727-42de-ae7d-3bb33155e148"` || json.Unmarshal(outer["data"], &fields) != nil || len(fields) != 1 || string(fields["last_opened_at"]) != `"`+data.LastOpenedAt.Format(time.RFC3339Nano)+`"` {
					t.Fatal("访问响应公开字段错误")
				}
			}
			baseAfter, err := store.FindUserFileEntry(ctx, actor.UserID, entryID)
			if err != nil || !reflect.DeepEqual(baseBefore, baseAfter) || !reflect.DeepEqual(starBefore, read(entryID).StarredAt) {
				t.Fatal("响应转换与基础或收藏事实混用", err)
			}
		}
	})
	// 4. 仅在测试事务连接固定数据库时钟，精确核对微秒级窗口边界。
	t.Run("exact sixty second boundaries", func(t *testing.T) {
		var fixed time.Time
		if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&fixed); err != nil {
			t.Fatal(err)
		}
		fixed = fixed.Truncate(time.Second)
		for _, testCase := range []struct {
			name   string
			age    time.Duration
			update bool
		}{
			{"one microsecond inside", 60*time.Second - time.Microsecond, false},
			{"exactly sixty seconds", 60 * time.Second, true},
			{"one microsecond outside", 60*time.Second + time.Microsecond, true},
			{"future fact", -time.Hour, false},
		} {
			t.Run(testCase.name, func(t *testing.T) {
				previous := fixed.Add(-testCase.age)
				var preference *domain.EntryPreference
				err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
					if err := txStore.database.WithContext(txContext).Exec("SET timestamp = ?", fixed.Unix()).Error; err != nil {
						return err
					}
					setupError := txStore.database.WithContext(txContext).Exec("UPDATE user_entry_preferences SET starred_at=?,last_opened_at=?,updated_at=? WHERE user_id=1 AND entry_id=13", old, previous, old).Error
					var writeError error
					if setupError == nil {
						preference, writeError = txStore.RecordUserEntryAccess(txContext, 13)
					}
					resetError := txStore.database.WithContext(txContext).Exec("SET timestamp = DEFAULT").Error
					return errors.Join(setupError, writeError, resetError)
				})
				wantOpened, wantUpdated := previous, old
				if testCase.update {
					wantOpened, wantUpdated = fixed, fixed
				}
				if err != nil || preference == nil || preference.LastOpenedAt == nil || !preference.LastOpenedAt.Equal(wantOpened) || !preference.StarredAt.Equal(old) || !audit(13)[1].Equal(wantUpdated) {
					t.Fatal("六十秒精确边界错误", err)
				}
				data, err := dto.NewRecordAccessData(preference)
				if err != nil || data == nil || !data.LastOpenedAt.Equal(wantOpened) {
					t.Fatal("精确窗口结果的访问响应错误", err)
				}
			})
		}
	})
	t.Run("scope type and hidden paths", func(t *testing.T) {
		for _, testCase := range []struct {
			id   uint64
			want error
		}{{33, domain.ErrFileEntryNotFound}, {999, domain.ErrFileEntryNotFound}, {11, domain.ErrFileEntryNotFile}, {12, domain.ErrFileEntryNotFile}} {
			result, err := record(ctx, testCase.id)
			if result != nil || !errors.Is(err, testCase.want) {
				t.Fatal("访问范围或文件类型错误", err)
			}
		}
		before := read(13)
		for _, entryID := range []uint64{12, 13} {
			for _, state := range []string{"TRASHED", "PURGING", "PURGED"} {
				if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status=?,deleted_at=UTC_TIMESTAMP(6),purge_started_at=IF(?='TRASHED',NULL,UTC_TIMESTAMP(6)),purged_at=IF(?='PURGED',UTC_TIMESTAMP(6),NULL),object_id=IF(entry_type='FILE' AND ?='PURGED',NULL,object_id) WHERE id=?", state, state, state, state, entryID); err != nil {
					t.Fatal(err)
				}
				result, operationError := record(ctx, 13)
				if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='ACTIVE',deleted_at=NULL,purge_started_at=NULL,purged_at=NULL,object_id=IF(entry_type='FILE',41,NULL) WHERE id=?", entryID); err != nil {
					t.Fatal(err)
				}
				if result != nil || !errors.Is(operationError, domain.ErrFileEntryNotFound) || !reflect.DeepEqual(before, read(13)) {
					t.Fatal("隐藏路径访问错误", operationError)
				}
			}
		}
	})

	// 5. 实际写入后失败或取消回滚，认证状态变化由事务复查。
	t.Run("rollback after write", func(t *testing.T) {
		for _, cancelAfter := range []bool{false, true} {
			if _, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET last_opened_at=?,updated_at=? WHERE user_id=1 AND entry_id=13", old, old); err != nil {
				t.Fatal(err)
			}
			before, stamps := read(13), audit(13)
			callContext, cancelCall := context.WithCancel(ctx)
			failure := errors.New("fixture access rollback")
			err := store.WithUserTransaction(callContext, actor, func(txContext context.Context, txStore *Store) error {
				if _, err := txStore.RecordUserEntryAccess(txContext, 13); err != nil {
					return err
				}
				if cancelAfter {
					cancelCall()
					return txContext.Err()
				}
				return failure
			})
			cancelCall()
			want := failure
			if cancelAfter {
				want = context.Canceled
			}
			if !errors.Is(err, want) || !reflect.DeepEqual(before, read(13)) || audit(13) != stamps {
				t.Fatal("访问写后回滚错误", err)
			}
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
			result, operationError := record(ctx, 13)
			if _, err := pool.ExecContext(ctx, item.restore); err != nil {
				t.Fatal(err)
			}
			if result != nil || !errors.Is(operationError, item.want) || !reflect.DeepEqual(before, read(13)) {
				t.Fatal("访问身份复查错误", operationError)
			}
		}
	})
	// 6. 旧快照后当前读回与并发访问复用同一数据库事实。
	t.Run("current read over old snapshot", func(t *testing.T) {
		var current *domain.EntryPreference
		newOpened := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
		err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			if _, err := txStore.FindUserEntryPreference(txContext, 1, 13); err != nil {
				return err
			}
			if _, err := pool.ExecContext(txContext, "UPDATE user_entry_preferences SET starred_at=?,last_opened_at=?,updated_at=UTC_TIMESTAMP(6) WHERE user_id=1 AND entry_id=13", old, newOpened); err != nil {
				return err
			}
			var err error
			current, err = txStore.RecordUserEntryAccess(txContext, 13)
			return err
		})
		if err != nil || current == nil || !current.LastOpenedAt.Equal(newOpened) || !current.StarredAt.Equal(old) {
			t.Fatal("当前偏好读取错误", err)
		}
	})
	t.Run("concurrent accesses", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET last_opened_at=? WHERE user_id=1 AND entry_id=13", old); err != nil {
			t.Fatal(err)
		}
		type outcome struct {
			preference *domain.EntryPreference
			err        error
		}
		results := make(chan outcome, 2)
		start := make(chan struct{})
		for range 2 {
			go func() { <-start; preference, err := record(ctx, 13); results <- outcome{preference, err} }()
		}
		close(start)
		first, second := <-results, <-results
		if first.err != nil || second.err != nil || first.preference == nil || second.preference == nil || first.preference.LastOpenedAt == nil || !reflect.DeepEqual(first.preference, second.preference) || !first.preference.LastOpenedAt.After(old) {
			t.Fatal("并发访问事实错误", first.err, second.err)
		}
		var count int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM user_entry_preferences WHERE user_id=1 AND entry_id=13").Scan(&count); err != nil || count != 1 {
			t.Fatal("并发偏好数量错误", err)
		}
	})
	t.Run("concurrent star and access", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET starred_at=NULL,last_opened_at=? WHERE user_id=1 AND entry_id=13", old); err != nil {
			t.Fatal(err)
		}
		results := make(chan error, 2)
		start := make(chan struct{})
		go func() {
			<-start
			_, err := record(ctx, 13)
			results <- err
		}()
		go func() {
			<-start
			results <- store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				_, err := txStore.SetUserEntryStarred(txContext, 13, true)
				return err
			})
		}()
		close(start)
		for range 2 {
			if err := <-results; err != nil {
				t.Fatal(err)
			}
		}
		preference := read(13)
		if preference.StarredAt == nil || preference.LastOpenedAt == nil || !preference.LastOpenedAt.After(old) {
			t.Fatal("并发收藏与访问事实错误")
		}
	})
	t.Run("service invalid result and cancellation rollback", func(t *testing.T) {
		for _, mode := range []string{"wrong owner", "wrong target", "missing access", "zero time", "cancel after write", "outer callback failure"} {
			t.Run(mode, func(t *testing.T) {
				if _, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET last_opened_at=?,updated_at=? WHERE user_id=1 AND entry_id=13", old, old); err != nil {
					t.Fatal(err)
				}
				before, beforeAudit := read(13), audit(13)
				callContext, cancelCall := context.WithCancel(ctx)
				defer cancelCall()
				failure := errors.New("fixture access outer failure")
				injected := service.NewRecordAccessService(func(ctx context.Context, actor domain.Actor, action func(context.Context, service.RecordAccessTransaction) error) error {
					return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
						err := action(txContext, &recordAccessResultStore{Store: txStore, afterRecord: func(preference *domain.EntryPreference) {
							switch mode {
							case "wrong owner":
								preference.UserID++
							case "wrong target":
								preference.EntryID++
							case "missing access":
								preference.LastOpenedAt = nil
							case "zero time":
								*preference.LastOpenedAt = time.Time{}
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
				result, err := injected.Record(callContext, actor, 13, "open")
				want := domain.ErrInvalidFileEntryData
				if mode == "cancel after write" {
					want = context.Canceled
				}
				if mode == "outer callback failure" {
					want = failure
				}
				if result != nil || !errors.Is(err, want) || !reflect.DeepEqual(read(13), before) || audit(13) != beforeAudit {
					t.Fatal("服务写后失败未完整回滚", err)
				}
			})
		}
	})
	t.Run("service confirmed commit then canceled", func(t *testing.T) {
		callContext, cancelCall := context.WithCancel(ctx)
		defer cancelCall()
		committed := service.NewRecordAccessService(func(ctx context.Context, actor domain.Actor, action func(context.Context, service.RecordAccessTransaction) error) error {
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error { return action(txContext, txStore) })
			if err == nil {
				cancelCall()
			}
			return err
		})
		result, err := committed.Record(callContext, actor, 13, "open")
		if err != nil || result == nil || callContext.Err() != context.Canceled || !reflect.DeepEqual(result, read(13)) {
			t.Fatal("已确认提交后的访问结果错误", err)
		}
	})
	t.Run("service rejects purpose before transaction", func(t *testing.T) {
		before, beforeAudit := read(13), audit(13)
		result, err := accessService.Record(ctx, actor, 13, "download")
		if result != nil || !errors.Is(err, domain.ErrInvalidEntryAccessPurpose) || !reflect.DeepEqual(before, read(13)) || audit(13) != beforeAudit {
			t.Fatal("非法访问用途未拒绝", err)
		}
	})
	// 7. 实际 JWT、认证中间件与 POST 处理函数连接真实访问事务。
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
		router.POST("/test-record-access/:id", middleware.RequireAuthorization(func(ctx context.Context, token string) (*domain.Actor, error) {
			current, err := authorization.Authorize(ctx, token)
			if err == nil && disableAfterAuthorization.Swap(false) {
				_, err = pool.ExecContext(ctx, "UPDATE users SET status='DISABLED' WHERE id=1")
			}
			return current, err
		}, handler.AuthorizationFailure), handler.RecordAccess(accessService.Record))
		server := httptest.NewServer(router)
		defer server.Close()
		client := server.Client()
		client.Timeout = 3 * time.Second
		requestNumber := 0
		request := func(id, body, token string, status int, code, field string) dto.RecordAccessResponse {
			t.Helper()
			requestNumber++
			requestID := fmt.Sprintf("28ee758a-0727-42de-ae7d-%012d", requestNumber)
			message, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/test-record-access/"+id+"?note=private-query", strings.NewReader(body))
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
				t.Fatal("真实访问 POST 失败", err)
			}
			encoded, readError := io.ReadAll(response.Body)
			closeError := response.Body.Close()
			if readError != nil || closeError != nil || response.StatusCode != status || response.Header.Get("X-Request-ID") != requestID || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Content-Type") != "application/json; charset=utf-8" {
				t.Fatal("真实访问响应状态或头错误", response.StatusCode, readError, closeError)
			}
			var result dto.RecordAccessResponse
			if status == 200 {
				if json.Unmarshal(encoded, &result) != nil || result.RequestID != requestID || result.Data.LastOpenedAt.IsZero() {
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
		starBefore := read(13).StarredAt
		baseBefore, err := store.FindUserFileEntry(ctx, 1, 13)
		if err != nil {
			t.Fatal(err)
		}
		first := request("13", `{"purpose":"preview"}`, access.Token, 200, "", "")
		repeated := request("13", `{"purpose":"open"}`, access.Token, 200, "", "")
		if !reflect.DeepEqual(first.Data, repeated.Data) || first.RequestID == repeated.RequestID || !first.Data.LastOpenedAt.Equal(*read(13).LastOpenedAt) {
			t.Fatal("真实访问合并或当前请求 ID 错误")
		}
		request("12", `{"purpose":"open"}`, access.Token, 400, "INVALID_ARGUMENT", "id")
		request("11", `{"purpose":"preview"}`, access.Token, 400, "INVALID_ARGUMENT", "id")
		request("33", `{"purpose":"open"}`, access.Token, 404, "ENTRY_NOT_FOUND", "")
		request("999", `{"purpose":"open"}`, access.Token, 404, "ENTRY_NOT_FOUND", "")
		before := read(13)
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id=12"); err != nil {
			t.Fatal(err)
		}
		request("13", `{"purpose":"preview"}`, access.Token, 404, "ENTRY_NOT_FOUND", "")
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='ACTIVE',deleted_at=NULL WHERE id=12"); err != nil {
			t.Fatal(err)
		}
		request("0", `{"private-broken":`, access.Token, 400, "INVALID_ARGUMENT", "id")
		request("13", `{}`, access.Token, 400, "INVALID_ARGUMENT", "purpose")
		request("13", `{"purpose":null}`, access.Token, 400, "INVALID_ARGUMENT", "purpose")
		request("13", `{"purpose":"download"}`, access.Token, 400, "INVALID_ARGUMENT", "purpose")
		request("0", `{"private-broken":`, "", 401, "AUTH_REQUIRED", "")
		request("13", `{"purpose":"open"}`, "invalid-access", 401, "AUTH_REQUIRED", "")
		disableAfterAuthorization.Store(true)
		request("13", `{"purpose":"open"}`, access.Token, 403, "ACCOUNT_DISABLED", "")
		if _, err := pool.ExecContext(ctx, "UPDATE users SET status='ACTIVE' WHERE id=1"); err != nil {
			t.Fatal(err)
		}
		baseAfter, err := store.FindUserFileEntry(ctx, 1, 13)
		if err != nil || !reflect.DeepEqual(baseBefore, baseAfter) || !reflect.DeepEqual(starBefore, read(13).StarredAt) || !reflect.DeepEqual(before, read(13)) {
			t.Fatal("真实访问错误边界或原有事实不符", err)
		}
	})
	t.Run("maximum identifiers revision and dates", func(t *testing.T) {
		maximum := ^uint64(0)
		for _, statement := range []struct {
			sql  string
			args []any
		}{
			{"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(?,'max_access','最大访问用户','fixture_hash','ACTIVE')", []any{maximum}},
			{"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,'ca67141f-9801-40be-aa25-d7cb9cdc74e6','最大设备','WEB')", []any{maximum, maximum}},
			{"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(?,?,?,UNHEX(REPEAT('22',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)", []any{maximum, maximum, maximum}},
			{"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,?,'MINIO','fixture-bucket','maximum-access',0)", []any{maximum, maximum}},
			{"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision) VALUES(?,?,NULL,'',X'','DIR',NULL,NULL,0,1),(?,?,?,'最大文件',CAST('最大文件' AS BINARY),'FILE','OTHER',?,0,?)", []any{maximum - 1, maximum, maximum, maximum, maximum - 1, maximum, maximum}},
		} {
			if _, err := pool.ExecContext(ctx, statement.sql, statement.args...); err != nil {
				t.Fatal(err)
			}
		}
		maximumActor := domain.Actor{UserID: maximum, DeviceID: maximum, SessionID: maximum}
		var result *domain.EntryPreference
		invoke := func() error {
			var err error
			result, err = accessService.Record(ctx, maximumActor, maximum, "open")
			return err
		}
		if err := invoke(); err != nil || result == nil || result.UserID != maximum || result.EntryID != maximum || result.LastOpenedAt == nil {
			t.Fatal("最大标识访问错误", err)
		}
		minimumStar, maximumOpened := time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
		if _, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET starred_at=?,last_opened_at=? WHERE user_id=? AND entry_id=?", minimumStar, maximumOpened, maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if err := invoke(); err != nil || result.StarredAt == nil || !result.StarredAt.Equal(minimumStar) || !result.LastOpenedAt.Equal(maximumOpened) {
			t.Fatal("日期边界或未来访问事实错误", err)
		}
		data, err := dto.NewRecordAccessData(result)
		if err != nil || data == nil || !data.LastOpenedAt.Equal(maximumOpened) {
			t.Fatal("最大标识及最高日期响应错误", err)
		}
	})
}
