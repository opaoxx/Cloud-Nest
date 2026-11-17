package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	serverhttp "github.com/opaoxx/cloud-nest/internal/server/http"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

// idempotentUploadSaveFailureStore 在真实结果保存后注入故障或取消。
type idempotentUploadSaveFailureStore struct {
	*Store
	failure error
	cancel  context.CancelFunc
}

func (store *idempotentUploadSaveFailureStore) CreateUserIdempotencyRecord(ctx context.Context, input domain.IdempotencyResultInput) (*domain.IdempotencyRecord, error) {
	record, err := store.Store.CreateUserIdempotencyRecord(ctx, input)
	if err != nil {
		return nil, err
	}
	if store.cancel != nil {
		store.cancel()
	}
	return record, store.failure
}

func TestIdempotentCreateUploadServiceIntegration(t *testing.T) {
	// 1. 在独占验证库中准备正式表、两位用户和当前账号的两个设备。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(3)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, boundary := range []struct{ table, next string }{{"auth_sessions", "stored_objects"}, {"upload_tasks", "idempotency_records"}, {"idempotency_records", ""}} {
		start := strings.Index(migrations.InitialSQL, "CREATE TABLE "+boundary.table+" (")
		end := len(migrations.InitialSQL)
		if boundary.next != "" {
			end = strings.Index(migrations.InitialSQL, "CREATE TABLE "+boundary.next+" (")
		}
		if start < 0 || end <= start {
			t.Fatal("正式幂等上传表边界错误")
		}
		if _, err := pool.ExecContext(ctx, migrations.InitialSQL[start:end]); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,storage_quota) VALUES(1,'idempotent_upload','幂等上传用户','fixture_hash',104857600),(3,'idempotent_upload_other','另一用户','fixture_hash',104857600)",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(13,1,'28ee758a-0727-42de-ae7d-3bb33155e148','原设备','WEB'),(14,1,'28ee758a-0727-42de-ae7d-3bb33155e149','另一设备','WINDOWS'),(31,3,'38ee758a-0727-42de-ae7d-3bb33155e148','另一用户设备','WINDOWS')",
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,1,13,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR),(18,1,14,UNHEX(REPEAT('22',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR),(37,3,31,UNHEX(REPEAT('33',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR'),(12,1,11,'target',X'746172676574','DIR')",
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
	sameUser := domain.Actor{UserID: 1, DeviceID: 14, SessionID: 18}
	other := domain.Actor{UserID: 3, DeviceID: 31, SessionID: 37}
	failure := errors.New("fixture idempotent creation rollback")
	encode := func(task *domain.UploadTask) (json.RawMessage, error) {
		data, err := dto.NewCreateUploadData(task)
		if err != nil {
			return nil, err
		}
		return json.Marshal(data)
	}
	validate := func(body json.RawMessage) error { _, err := dto.RestoreCreateUploadData(body); return err }
	newCreator := func(mode string, cancelCall context.CancelFunc) *service.IdempotentCreateUploadService {
		withTransaction := func(ctx context.Context, current domain.Actor, action func(context.Context, service.IdempotentCreateUploadTransaction) error) error {
			return store.WithUserTransaction(ctx, current, func(txContext context.Context, txStore *Store) error {
				var transaction service.IdempotentCreateUploadTransaction = txStore
				switch mode {
				case "reserve", "object", "task":
					transaction = &createUploadServiceStore{Store: txStore, failStage: mode, failure: failure, cancel: cancelCall}
				case "save":
					transaction = &idempotentUploadSaveFailureStore{Store: txStore, failure: failure, cancel: cancelCall}
				}
				if err := action(txContext, transaction); err != nil {
					return err
				}
				if mode == "outer failure" {
					return failure
				}
				return nil
			})
		}
		encoder := encode
		if mode == "encode" {
			encoder = func(*domain.UploadTask) (json.RawMessage, error) { return nil, failure }
		}
		validator := validate
		if mode == "validate encoded" || mode == "validate saved" {
			calls := 0
			validator = func(body json.RawMessage) error {
				calls++
				if (mode == "validate encoded" && calls == 1) || (mode == "validate saved" && calls == 2) {
					return failure
				}
				return validate(body)
			}
		}
		return service.NewIdempotentCreateUploadService("local_minio", "cloudnest", withTransaction, encoder, validator)
	}
	creator := newCreator("", nil)
	inputFor := func() domain.CreateUploadInput {
		return domain.CreateUploadInput{ParentID: 12, Name: " 学习.bin ", SizeBytes: 40}
	}
	type uploadState struct{ reserved, taskReserved, objects, tasks, records, used, sequence uint64 }
	readState := func(userID uint64) uploadState {
		var state uploadState
		if err := pool.QueryRowContext(ctx, "SELECT reserved_storage,(SELECT COALESCE(SUM(reserved_bytes),0) FROM upload_tasks WHERE user_id=users.id),(SELECT COUNT(*) FROM stored_objects WHERE owner_user_id=users.id),(SELECT COUNT(*) FROM upload_tasks WHERE user_id=users.id),(SELECT COUNT(*) FROM idempotency_records WHERE user_id=users.id),used_storage,change_seq FROM users WHERE id=?", userID).Scan(&state.reserved, &state.taskReserved, &state.objects, &state.tasks, &state.records, &state.used, &state.sequence); err != nil {
			t.Fatal(err)
		}
		return state
	}

	// 2. 单连接完成四项提交，重试得到相同正文并保持原预占。
	t.Run("one connection create and replay", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		for _, mode := range []string{"ordinary", "zero bytes", "single limit", "digest", "zero digest"} {
			input := inputFor()
			switch mode {
			case "zero bytes":
				input.SizeBytes = 0
			case "single limit":
				input.SizeBytes = domain.MaxSingleUploadSizeBytes
			case "digest":
				digest := sha256.Sum256([]byte("fixture-upload-content"))
				input.ExpectedSHA256 = &digest
			case "zero digest":
				input.ExpectedSHA256 = new([32]byte)
			}
			key := "idempotent-upload-" + strings.ReplaceAll(mode, " ", "-")
			before := readState(actor.UserID)
			first, err := creator.Create(ctx, actor, input, key)
			if err != nil {
				t.Fatal(err)
			}
			data, err := dto.RestoreCreateUploadData(first)
			if err != nil {
				t.Fatal(err)
			}
			after := readState(actor.UserID)
			if after.reserved != before.reserved+input.SizeBytes || after.reserved != after.taskReserved || after.objects != before.objects+1 || after.tasks != before.tasks+1 || after.records != before.records+1 || after.used != before.used || after.sequence != before.sequence {
				t.Fatal("首次创建四项数据未一起提交")
			}
			input.Name = "学习.bin"
			second, err := creator.Create(ctx, actor, input, key)
			if err != nil || !bytes.Equal(first, second) || readState(actor.UserID) != after {
				t.Fatal("重试重复创建或重复预占", err)
			}
			record, err := store.FindUserIdempotencyRecord(ctx, actor.UserID, domain.CreateUploadOperation, key)
			if err != nil || record.ResourceID == nil || strconv.FormatUint(*record.ResourceID, 10) != data.Task.ID || record.ExpiresAt.Sub(record.CreatedAt) < 7*24*time.Hour {
				t.Fatal("已存任务关联或保留期错误", err)
			}
		}
	})
	t.Run("same user other device and separate account", func(t *testing.T) {
		input := inputFor()
		const key = "idempotent-upload-ordinary"
		before := readState(actor.UserID)
		first, err := creator.Create(ctx, actor, input, key)
		if err != nil {
			t.Fatal(err)
		}
		replayed, err := creator.Create(ctx, sameUser, input, key)
		if err != nil || !bytes.Equal(first, replayed) || readState(actor.UserID) != before {
			t.Fatal("同账号其他设备未复用原结果", err)
		}
		input.ParentID = 33
		otherBody, err := creator.Create(ctx, other, input, key)
		if err != nil || bytes.Equal(first, otherBody) || readState(actor.UserID) != before || readState(other.UserID).reserved != input.SizeBytes {
			t.Fatal("同键跨账号未独立创建", err)
		}
	})
	t.Run("request conflict and different keys", func(t *testing.T) {
		before := readState(actor.UserID)
		for _, input := range []domain.CreateUploadInput{
			{ParentID: 11, Name: "学习.bin", SizeBytes: 40}, {ParentID: 12, Name: "新名称.bin", SizeBytes: 40}, {ParentID: 12, Name: "学习.bin", SizeBytes: 41}, {ParentID: 12, Name: "学习.bin", SizeBytes: 40, ExpectedSHA256: new([32]byte)},
		} {
			body, err := creator.Create(ctx, actor, input, "idempotent-upload-ordinary")
			if body != nil || !errors.Is(err, domain.ErrIdempotencyRequestConflict) || readState(actor.UserID) != before {
				t.Fatal("同键不同参数未冲突或产生新写入", err)
			}
		}
		if _, err := creator.Create(ctx, actor, inputFor(), "independent-key"); err != nil {
			t.Fatal("不同键的同名上传创建失败", err)
		}
		after := readState(actor.UserID)
		if after.reserved != before.reserved+40 || after.tasks != before.tasks+1 || after.records != before.records+1 {
			t.Fatal("不同键未形成独立尝试")
		}
	})
	t.Run("historical snapshot and rebuilt service", func(t *testing.T) {
		input := inputFor()
		const key = "idempotent-upload-ordinary"
		first, err := creator.Create(ctx, actor, input, key)
		if err != nil {
			t.Fatal(err)
		}
		data, err := dto.RestoreCreateUploadData(first)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET status='VERIFYING',expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE id=?", data.Task.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id=12"); err != nil {
			t.Fatal(err)
		}
		before := readState(actor.UserID)
		reopened, err := NewStore(pool)
		if err != nil {
			t.Fatal(err)
		}
		rebuilt := service.NewIdempotentCreateUploadService("local_minio", "cloudnest", func(ctx context.Context, current domain.Actor, action func(context.Context, service.IdempotentCreateUploadTransaction) error) error {
			return reopened.WithUserTransaction(ctx, current, func(txContext context.Context, txStore *Store) error { return action(txContext, txStore) })
		}, encode, validate)
		replayed, err := rebuilt.Create(ctx, actor, input, key)
		if _, restoreError := pool.ExecContext(ctx, "UPDATE file_entries SET status='ACTIVE',deleted_at=NULL WHERE id=12"); restoreError != nil {
			t.Fatal(restoreError)
		}
		if err != nil || !bytes.Equal(first, replayed) || readState(actor.UserID) != before {
			t.Fatal("重建后未复用历史创建快照", err)
		}
	})

	t.Run("committed upload result delivered through HTTP", func(t *testing.T) {
		var logs bytes.Buffer
		const origin = "http://127.0.0.1:5173"
		router := serverhttp.NewRouter(logging.New(&logs), []string{origin}, func(context.Context) bool { return true })
		router.POST("/test-create-upload-success/:mode", func(requestContext *gin.Context) {
			input := inputFor()
			mode := requestContext.Param("mode")
			key := "delivery-" + mode
			switch mode {
			case "zero":
				input.SizeBytes = 0
			case "limit":
				input.SizeBytes = domain.MaxSingleUploadSizeBytes
			case "history":
				key = "idempotent-upload-ordinary"
			}
			stableBody, err := creator.Create(requestContext.Request.Context(), actor, input, key)
			if err != nil {
				handler.InternalError(requestContext, nil)
				return
			}
			requestContext.Header("Content-Length", "3")
			requestContext.Header("Content-Encoding", "gzip")
			handler.CreateUploadSuccess(requestContext, stableBody)
		}, func(*gin.Context) { t.Error("上传创建交付后进入下游") })
		server := httptest.NewServer(router)
		defer server.Close()
		client := server.Client()
		client.Timeout = 3 * time.Second
		sequence := 0
		for _, mode := range []string{"ordinary", "zero", "limit", "history"} {
			var previousData *dto.CreateUploadData
			var afterFirst uploadState
			before := readState(actor.UserID)
			for attempt := 0; attempt < 2; attempt++ {
				sequence++
				request, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/test-create-upload-success/"+mode+"?note=private-delivery-query", strings.NewReader("private-delivery-body"))
				if err != nil {
					t.Fatal(err)
				}
				requestID := "28ee758a-0727-42de-ae7d-" + strconv.FormatInt(300000000000+int64(sequence), 10)
				request.Header.Set("X-Request-ID", strings.ToUpper(requestID))
				request.Header.Set("Origin", origin)
				request.Header.Set("Authorization", "Bearer private-delivery-access")
				request.Header.Set("Idempotency-Key", "private-delivery-key")
				request.AddCookie(&http.Cookie{Name: handler.RefreshCookieName, Value: "private-delivery-cookie"})
				response, err := client.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				encoded, readError := io.ReadAll(response.Body)
				closeError := response.Body.Close()
				if readError != nil || closeError != nil || response.StatusCode != 201 || response.ContentLength != int64(len(encoded)) ||
					response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Content-Encoding") != "" ||
					response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Pragma") != "no-cache" || response.Header.Get("X-Request-ID") != requestID ||
					response.Header.Get("Access-Control-Allow-Origin") != origin || len(response.Cookies()) != 0 {
					t.Fatal("正式创建结果 HTTP 状态、正文或响应头错误", readError, closeError)
				}
				var body dto.CreateUploadResponse
				if json.Unmarshal(encoded, &body) != nil || body.RequestID != requestID || body.Data.Task.Status != "UPLOADING" || strings.Contains(string(encoded), "private-delivery") {
					t.Fatal("正式上传创建成功 JSON 错误")
				}
				current := readState(actor.UserID)
				if attempt == 0 {
					previousData = &body.Data
					afterFirst = current
					if mode == "history" {
						if current != before {
							t.Fatal("历史快照交付产生了重复创建")
						}
					} else if current.tasks != before.tasks+1 || current.objects != before.objects+1 || current.records != before.records+1 || current.reserved != before.reserved+body.Data.Task.SizeBytes || current.reserved != current.taskReserved {
						t.Fatal("成功交付前四项创建事实不一致")
					}
				} else if !reflect.DeepEqual(previousData, &body.Data) || current != afterFirst {
					t.Fatal("同快照再次交付发生变化或重复预占")
				}
			}
		}
		lines := bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n"))
		if len(lines) != sequence {
			t.Fatal("真实创建交付完成日志数量错误")
		}
		for index, line := range lines {
			var completed map[string]any
			expectedID := "28ee758a-0727-42de-ae7d-" + strconv.FormatInt(300000000001+int64(index), 10)
			if json.Unmarshal(line, &completed) != nil || completed["status"] != float64(201) || completed["method"] != "POST" || completed["level"] != "INFO" || completed["request_id"] != expectedID {
				t.Fatal("真实创建交付完成日志错误")
			}
		}
		if strings.Contains(logs.String(), "private-delivery") || strings.Contains(logs.String(), "学习.bin") {
			t.Fatal("完成日志包含原始请求或业务字段")
		}
	})

	// 3. 真实写入后的故障与取消必须回滚预占、对象、任务和幂等记录。
	for _, mode := range []string{"reserve", "object", "task", "encode", "validate encoded", "save", "validate saved", "outer failure"} {
		t.Run(mode+" rollback", func(t *testing.T) {
			before := readState(actor.UserID)
			body, err := newCreator(mode, nil).Create(ctx, actor, inputFor(), "rollback-"+strings.ReplaceAll(mode, " ", "-"))
			if body != nil || !errors.Is(err, failure) || readState(actor.UserID) != before {
				t.Fatal("失败后四项事实未完整回滚", err)
			}
		})
	}
	for _, mode := range []string{"reserve", "object", "task", "save"} {
		t.Run(mode+" cancellation", func(t *testing.T) {
			before := readState(actor.UserID)
			callContext, cancelCall := context.WithCancel(ctx)
			defer cancelCall()
			body, err := newCreator(mode, cancelCall).Create(callContext, actor, inputFor(), "cancel-"+mode)
			if body != nil || !errors.Is(err, context.Canceled) || readState(actor.UserID) != before {
				t.Fatal("取消后四项事实未完整回滚", err)
			}
		})
	}
	for _, fixture := range []struct {
		name, change, restore string
		want                  error
	}{
		{"disabled account", "UPDATE users SET status='DISABLED' WHERE id=1", "UPDATE users SET status='ACTIVE' WHERE id=1", domain.ErrAccountDisabled},
		{"revoked device", "UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=13", "UPDATE user_devices SET revoked_at=NULL WHERE id=13", domain.ErrDeviceRevoked},
		{"revoked session", "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=17", "UPDATE auth_sessions SET revoked_at=NULL WHERE id=17", domain.ErrSessionRevoked},
		{"expired session", "UPDATE auth_sessions SET expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE id=17", "UPDATE auth_sessions SET expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=17", domain.ErrSessionExpired},
	} {
		t.Run(fixture.name+" blocks replay", func(t *testing.T) {
			before := readState(actor.UserID)
			if _, err := pool.ExecContext(ctx, fixture.change); err != nil {
				t.Fatal(err)
			}
			body, err := creator.Create(ctx, actor, inputFor(), "idempotent-upload-ordinary")
			if _, restoreError := pool.ExecContext(ctx, fixture.restore); restoreError != nil {
				t.Fatal(restoreError)
			}
			if body != nil || !errors.Is(err, fixture.want) || readState(actor.UserID) != before {
				t.Fatal("失效身份仍获得重试结果", err)
			}
		})
	}
	t.Run("expired record and invalid snapshot", func(t *testing.T) {
		for _, mode := range []string{"expired", "invalid snapshot"} {
			key := "saved-" + strings.ReplaceAll(mode, " ", "-")
			if _, err := creator.Create(ctx, actor, inputFor(), key); err != nil {
				t.Fatal(err)
			}
			if mode == "expired" {
				if _, err := pool.ExecContext(ctx, "UPDATE idempotency_records SET created_at=UTC_TIMESTAMP(6)-INTERVAL 2 DAY,expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 DAY WHERE user_id=1 AND operation=? AND idempotency_key=?", domain.CreateUploadOperation, key); err != nil {
					t.Fatal(err)
				}
			} else if _, err := pool.ExecContext(ctx, "UPDATE idempotency_records SET response_body=JSON_SET(response_body,'$.content_path','/invalid') WHERE user_id=1 AND operation=? AND idempotency_key=?", domain.CreateUploadOperation, key); err != nil {
				t.Fatal(err)
			}
			before := readState(actor.UserID)
			body, err := creator.Create(ctx, actor, inputFor(), key)
			want := domain.ErrIdempotencyRecordExpired
			if mode == "invalid snapshot" {
				want = domain.ErrInvalidIdempotencyRecordData
			}
			if body != nil || !errors.Is(err, want) || readState(actor.UserID) != before {
				t.Fatal("异常记录未拒绝或触发了新的创建", err)
			}
		}
	})

	// 4. 实际并发覆盖同键、同键参数冲突和不同键配额竞争。
	for _, mode := range []string{"same key", "conflicting key", "quota"} {
		t.Run("concurrent "+mode, func(t *testing.T) {
			before := readState(actor.UserID)
			if mode == "quota" {
				if _, err := pool.ExecContext(ctx, "UPDATE users SET storage_quota=? WHERE id=1", before.reserved+100); err != nil {
					t.Fatal(err)
				}
			}
			type result struct {
				body json.RawMessage
				err  error
			}
			results := make(chan result, 2)
			start := make(chan struct{})
			var workers sync.WaitGroup
			for attempt := 0; attempt < 2; attempt++ {
				workers.Add(1)
				go func(attempt int) {
					defer workers.Done()
					<-start
					input := inputFor()
					key := "concurrent-" + strings.ReplaceAll(mode, " ", "-")
					if mode == "conflicting key" {
						input.SizeBytes += uint64(attempt)
					}
					if mode == "quota" {
						input.SizeBytes = 60
						key += "-" + strconv.Itoa(attempt)
					}
					body, err := creator.Create(ctx, actor, input, key)
					results <- result{body: body, err: err}
				}(attempt)
			}
			close(start)
			workers.Wait()
			close(results)
			successes, failures := 0, 0
			var firstBody json.RawMessage
			var successfulSize uint64
			for result := range results {
				if result.err == nil {
					successes++
					data, err := dto.RestoreCreateUploadData(result.body)
					if err != nil {
						t.Fatal(err)
					}
					successfulSize = data.Task.SizeBytes
					if firstBody != nil && !bytes.Equal(firstBody, result.body) {
						t.Fatal("同键并发返回不同结果")
					}
					firstBody = result.body
				} else {
					want := domain.ErrIdempotencyRequestConflict
					if mode == "quota" {
						want = domain.ErrQuotaExceeded
					}
					if !errors.Is(result.err, want) {
						t.Fatal(result.err)
					}
					failures++
				}
			}
			expectedSuccess := 1
			if mode == "same key" {
				expectedSuccess = 2
			}
			after := readState(actor.UserID)
			if successes != expectedSuccess || failures != 2-expectedSuccess || after.reserved != before.reserved+successfulSize || after.reserved != after.taskReserved || after.objects != before.objects+1 || after.tasks != before.tasks+1 || after.records != before.records+1 {
				t.Fatal("并发幂等或配额结果不一致")
			}
		})
	}
	t.Run("maximum identities and high generated IDs", func(t *testing.T) {
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash,storage_quota) VALUES(?,'idempotent_upload_max','完整用户','fixture_hash',?)", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,'48ee758a-0727-42de-ae7d-3bb33155e148','完整设备','WINDOWS')", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(?,?,?,UNHEX(REPEAT('44',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)", maximum, maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		for _, statement := range []string{"ALTER TABLE stored_objects AUTO_INCREMENT = 9223372036854775810", "ALTER TABLE upload_tasks AUTO_INCREMENT = 9223372036854775810"} {
			if _, err := pool.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
		current := domain.Actor{UserID: maximum, DeviceID: maximum, SessionID: maximum}
		input := domain.CreateUploadInput{ParentID: maximum, Name: "maximum.bin", SizeBytes: 0}
		first, err := creator.Create(ctx, current, input, "maximum-key")
		if err != nil {
			t.Fatal(err)
		}
		data, err := dto.RestoreCreateUploadData(first)
		if err != nil || data.Task.ParentID != "18446744073709551615" {
			t.Fatal("完整父目录标识丢失", err)
		}
		taskID, err := strconv.ParseUint(data.Task.ID, 10, 64)
		if err != nil || taskID < uint64(1)<<63 {
			t.Fatal("高位任务标识丢失", err)
		}
		task, err := store.FindUserUploadTask(ctx, maximum, taskID)
		if err != nil || task.UserID != maximum || task.DeviceID != maximum || task.StagedObjectID < uint64(1)<<63 {
			t.Fatal("完整身份或对象标识丢失", err)
		}
		second, err := creator.Create(ctx, current, input, "maximum-key")
		state := readState(maximum)
		if err != nil || !bytes.Equal(first, second) || state.objects != 1 || state.tasks != 1 || state.records != 1 || state.reserved != 0 {
			t.Fatal("完整整数重试产生了重复事实", err)
		}
	})
}
