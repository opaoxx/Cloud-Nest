package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// uploadTaskReadRows 复用可报告行错误的读取流程及已有任务列清单。
type uploadTaskReadRows struct{ *idempotencyRecordRows }

func (*uploadTaskReadRows) Columns() []string { return (&uploadTaskMappingRows{}).Columns() }

// uploadTaskReadFixture 提供具有独立时间和完整可空事实的已完成分片任务。
func uploadTaskReadFixture() (domain.UploadTask, []driver.Value) {
	stamp := time.Date(2026, 11, 17, 8, 0, 0, 123456000, time.UTC)
	publishedID, resultID, partSize, partCount := uint64(29), uint64(31), uint64(5242880), uint32(3)
	resolvedName, storageID, leaseOwner := "发布笔记.MD", "storage-session", "worker-1"
	errorCode, errorMessage := "STORAGE_RETRY", "曾经重试"
	leaseUntil, nextAttemptAt, completedAt := stamp.Add(time.Minute), stamp.Add(2*time.Minute), stamp.Add(3*time.Minute)
	digest := sha256.Sum256([]byte("upload fixture"))
	want := domain.UploadTask{
		ID: 19, UserID: 7, DeviceID: 11, StagedObjectID: 23, PublishedObjectID: &publishedID,
		TargetParentID: 13, RequestedName: "学习笔记.MD", ResolvedName: &resolvedName,
		UploadMode: "MULTIPART", StorageUploadID: &storageID, ExpectedSHA256: &digest,
		FileSize: 16777216, PartSize: &partSize, PartCount: &partCount, Status: domain.UploadStatusCompleted,
		ResultEntryID: &resultID, LeaseOwner: &leaseOwner, LeaseUntil: &leaseUntil, LeaseVersion: 3,
		NextAttemptAt: &nextAttemptAt, AttemptCount: 2, ExpiresAt: stamp.Add(24 * time.Hour),
		LastErrorCode: &errorCode, LastErrorMessage: &errorMessage,
		CreatedAt: stamp, UpdatedAt: stamp.Add(time.Second), CompletedAt: &completedAt,
	}
	values := []driver.Value{
		int64(19), int64(7), int64(11), int64(23), int64(29), int64(13),
		want.RequestedName, resolvedName, want.UploadMode, storageID, append([]byte(nil), digest[:]...),
		int64(16777216), int64(5242880), int64(3), "COMPLETED", int64(31), int64(0),
		leaseOwner, leaseUntil, int64(3), nextAttemptAt, int64(2), want.ExpiresAt,
		errorCode, errorMessage, want.CreatedAt, want.UpdatedAt, completedAt,
		nil, nil, nil, nil,
	}
	return want, values
}

func TestFindUserUploadTaskResults(t *testing.T) {
	modes := []string{"full record", "nullable SINGLE", "zero digest", "expired retained", "maximum unsigned", "UTC conversion", "time boundaries", "creating multipart without session", "missing", "query failure", "row failure"}
	for _, status := range []string{"CREATING", "UPLOADING", "COMPLETING", "VERIFYING", "COMPLETED", "CANCELLING", "CANCELLED", "EXPIRING", "EXPIRED", "FAILED"} {
		modes = append(modes, status)
	}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			// 1. 准备合法任务及本组范围、空值或状态。
			want, values := uploadTaskReadFixture()
			fault := errors.New("private upload query failure")
			var queryError, rowsError, wantError error
			switch mode {
			case "nullable SINGLE":
				want = domain.UploadTask{ID: 19, UserID: 7, DeviceID: 11, StagedObjectID: 23, TargetParentID: 13,
					RequestedName: "学习笔记.MD", UploadMode: "SINGLE", Status: domain.UploadStatusUploading,
					CreatedAt: want.CreatedAt, UpdatedAt: want.UpdatedAt, ExpiresAt: want.ExpiresAt}
				for _, column := range []int{4, 7, 9, 10, 12, 13, 15, 17, 18, 20, 23, 24, 27} {
					values[column] = nil
				}
				values[8], values[11], values[14], values[19], values[21] = "SINGLE", int64(0), "UPLOADING", int64(0), int64(0)
			case "zero digest":
				want.ExpectedSHA256, values[10] = new([sha256.Size]byte), make([]byte, sha256.Size)
			case "expired retained":
				want.ExpiresAt = want.CreatedAt.Add(-24 * time.Hour)
				values[22] = want.ExpiresAt
			case "maximum unsigned":
				maximum := ^uint64(0)
				want.ID, want.UserID, want.DeviceID, want.StagedObjectID, want.TargetParentID = maximum, maximum, maximum, maximum, maximum
				*want.PublishedObjectID, *want.ResultEntryID, *want.PartSize, *want.PartCount = maximum, maximum, maximum, 10000
				want.FileSize, want.LeaseVersion, want.AttemptCount = maximum, maximum, ^uint32(0)
				for _, column := range []int{0, 1, 2, 3, 4, 5, 11, 12, 15, 19} {
					values[column] = "18446744073709551615"
				}
				values[13], values[21] = int64(10000), "4294967295"
			case "UTC conversion":
				zone := time.FixedZone("fixture", 8*60*60)
				for _, column := range []int{18, 20, 22, 25, 26, 27} {
					values[column] = values[column].(time.Time).In(zone)
				}
			case "time boundaries":
				want.CreatedAt = time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
				want.UpdatedAt = time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
				values[25], values[26] = want.CreatedAt, want.UpdatedAt
			case "creating multipart without session":
				want.Status, want.StorageUploadID = domain.UploadStatusCreating, nil
				values[14], values[9] = "CREATING", nil
			case "missing":
				values, wantError = nil, domain.ErrUploadTaskNotFound
			case "query failure":
				queryError, wantError = fault, fault
			case "row failure":
				rowsError, wantError = fault, fault
			default:
				if domain.UploadStatus(mode).IsValid() {
					want.Status, values[14] = domain.UploadStatus(mode), mode
				}
			}
			// 2. 核对显式投影、用户/任务参数、单条上限及期限。
			queries := 0
			connector := &deviceConnector{accountConnector: accountConnector{query: func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				queries++
				expectedSQL := "SELECT " + strings.Join((&uploadTaskMappingRows{}).Columns(), ",") + " FROM upload_tasks WHERE user_id = ? AND id = ? LIMIT ?"
				actualSQL := strings.Join(strings.Fields(strings.ReplaceAll(statement, "`", "")), " ")
				if actualSQL != expectedSQL || len(arguments) != 3 || arguments[0].Value != want.UserID || arguments[1].Value != want.ID || arguments[2].Value != int64(1) {
					t.Error("上传任务投影或绑定范围错误")
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
					t.Error("上传任务查询期限错误")
				}
				return &uploadTaskReadRows{&idempotencyRecordRows{values: values, err: rowsError}}, queryError
			}}}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			// 3. 成功返回完整事实，故障保留原因且整体返回 nil。
			actual, err := store.FindUserUploadTask(context.Background(), want.UserID, want.ID)
			if wantError != nil {
				if actual != nil || !errors.Is(err, wantError) {
					t.Fatal("上传任务缺失或故障处理错误", err)
				}
			} else if err != nil || !reflect.DeepEqual(actual, &want) {
				t.Fatal("上传任务事实未完整读回", err)
			}
			if queries != 1 || pool.PingContext(context.Background()) != nil {
				t.Fatal("上传任务查询次数或共享连接池生命周期错误")
			}
		})
	}
}

func TestFindUserUploadTaskInvalidRecords(t *testing.T) {
	fixtures := []struct {
		name    string
		changes map[int]driver.Value
	}{
		{"wrong id", map[int]driver.Value{0: int64(20)}},
		{"wrong owner", map[int]driver.Value{1: int64(8)}},
		{"zero device", map[int]driver.Value{2: int64(0)}},
		{"zero staged object", map[int]driver.Value{3: int64(0)}},
		{"zero target", map[int]driver.Value{5: int64(0)}},
		{"zero published object", map[int]driver.Value{4: int64(0)}},
		{"zero result", map[int]driver.Value{15: int64(0)}},
		{"empty name", map[int]driver.Value{6: ""}},
		{"invalid name encoding", map[int]driver.Value{6: "\xff"}},
		{"empty resolved name", map[int]driver.Value{7: ""}},
		{"invalid resolved encoding", map[int]driver.Value{7: "\xff"}},
		{"empty digest", map[int]driver.Value{10: []byte{}}},
		{"short digest", map[int]driver.Value{10: make([]byte, 31)}},
		{"long digest", map[int]driver.Value{10: make([]byte, 33)}},
		{"unknown status", map[int]driver.Value{14: "UNKNOWN"}},
		{"lowercase status", map[int]driver.Value{14: "completed"}},
		{"unknown mode", map[int]driver.Value{8: "UNKNOWN"}},
		{"SINGLE part size", map[int]driver.Value{8: "SINGLE", 9: nil, 13: nil}},
		{"SINGLE part count", map[int]driver.Value{8: "SINGLE", 9: nil, 12: nil}},
		{"SINGLE storage session", map[int]driver.Value{8: "SINGLE", 12: nil, 13: nil}},
		{"missing part size", map[int]driver.Value{12: nil}},
		{"small part size", map[int]driver.Value{12: int64(5242879)}},
		{"missing part count", map[int]driver.Value{13: nil}},
		{"zero part count", map[int]driver.Value{13: int64(0)}},
		{"too many parts", map[int]driver.Value{13: int64(10001)}},
		{"reservation exceeds file", map[int]driver.Value{14: "UPLOADING", 16: int64(16777217)}},
		{"terminal reservation", map[int]driver.Value{16: int64(1)}},
		{"completed missing object", map[int]driver.Value{4: nil}},
		{"completed missing result", map[int]driver.Value{15: nil}},
		{"completed missing time", map[int]driver.Value{27: nil}},
		{"lease missing owner", map[int]driver.Value{17: nil}},
		{"lease missing deadline", map[int]driver.Value{18: nil}},
		{"invalid error encoding", map[int]driver.Value{24: "\xff"}},
		{"submicrosecond time", map[int]driver.Value{22: time.Date(2026, 11, 17, 8, 0, 0, 1, time.UTC)}},
		{"below database year", map[int]driver.Value{22: time.Date(999, 12, 31, 23, 59, 59, 0, time.UTC)}},
		{"above database year", map[int]driver.Value{22: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}},
		{"partial replacement target", map[int]driver.Value{28: "31"}},
		{"partial replacement revision", map[int]driver.Value{29: "4"}},
		{"zero replacement target", map[int]driver.Value{28: "0", 29: "4", 30: "5"}},
		{"zero replacement revision", map[int]driver.Value{28: "31", 29: "0", 30: "1"}},
		{"replacement missing digest", map[int]driver.Value{10: nil, 28: "31", 29: "4", 30: "5"}},
		{"replacement wrong result entry", map[int]driver.Value{28: "32", 29: "4", 30: "5"}},
		{"replacement completed missing version", map[int]driver.Value{28: "31", 29: "4"}},
		{"replacement completed version skipped", map[int]driver.Value{28: "31", 29: "4", 30: "6"}},
		{"replacement version cannot wrap", map[int]driver.Value{28: "31", 29: "18446744073709551615", 30: "0"}},
		{"replacement premature version", map[int]driver.Value{14: "VERIFYING", 15: nil, 28: "31", 29: "4", 30: "5"}},
		{"replacement unexpected result entry", map[int]driver.Value{14: "VERIFYING", 28: "31", 29: "4"}},
		{"replacement conflict missing version", map[int]driver.Value{14: "FAILED", 15: nil, 23: domain.UploadRevisionConflictCode, 28: "31", 29: "4"}},
		{"replacement conflict equals expectation", map[int]driver.Value{14: "FAILED", 15: nil, 23: domain.UploadRevisionConflictCode, 28: "31", 29: "4", 31: "4"}},
		{"replacement unexpected conflict version", map[int]driver.Value{14: "FAILED", 15: nil, 28: "31", 29: "4", 31: "5"}},
		{"ordinary upload with replacement outcome", map[int]driver.Value{30: "5"}},
	}
	for _, column := range []int{18, 20, 22, 25, 26, 27} {
		fixtures = append(fixtures, struct {
			name    string
			changes map[int]driver.Value
		}{"zero " + (&uploadTaskMappingRows{}).Columns()[column], map[int]driver.Value{column: time.Time{}}})
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 每组只替换相关事实，核对固定数据异常与 nil 结果。
			_, values := uploadTaskReadFixture()
			for column, value := range fixture.changes {
				values[column] = value
			}
			connector := &deviceConnector{accountConnector: accountConnector{query: func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
				return &uploadTaskReadRows{&idempotencyRecordRows{values: values}}, nil
			}}}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := store.FindUserUploadTask(context.Background(), 7, 19)
			if actual != nil || !errors.Is(err, domain.ErrInvalidUploadTaskData) || err.Error() != "上传任务数据异常" {
				t.Fatal("异常上传事实被接受或错误提示泄露原值", err)
			}
		})
	}
}

func TestFindUserUploadTaskInput(t *testing.T) {
	// 1. 非法范围及预先取消在连接数据库之前结束。
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, release := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer release()
	connector := &deviceConnector{}
	pool := sql.OpenDB(connector)
	defer pool.Close()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		ctx            context.Context
		userID, taskID uint64
		wantError      error
	}{
		{userID: 7, taskID: 19},
		{ctx: context.Background(), taskID: 19},
		{ctx: context.Background(), userID: 7},
		{ctx: cancelled, userID: 7, taskID: 19, wantError: context.Canceled},
		{ctx: expired, userID: 7, taskID: 19, wantError: context.DeadlineExceeded},
	} {
		actual, err := store.FindUserUploadTask(fixture.ctx, fixture.userID, fixture.taskID)
		if actual != nil || err == nil || (fixture.wantError != nil && !errors.Is(err, fixture.wantError)) || connector.connections.Load() != 0 {
			t.Fatal("无效调用访问数据库或丢失取消原因", err)
		}
	}
}

func TestFindUserUploadTaskCancellation(t *testing.T) {
	for _, phase := range []string{"caller deadline", "default deadline", "after result", "after missing", "after failure"} {
		t.Run(phase, func(t *testing.T) {
			// 1. 覆盖调用方更短期限、默认五秒及查询返回时的取消。
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if phase == "caller deadline" {
				var release context.CancelFunc
				ctx, release = context.WithTimeout(ctx, 30*time.Millisecond)
				defer release()
			}
			connector := &deviceConnector{accountConnector: accountConnector{query: func(queryContext context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
				if strings.HasPrefix(phase, "after") {
					cancel()
					if phase == "after failure" {
						return nil, errors.New("private cancelled upload query")
					}
					var values []driver.Value
					if phase == "after result" {
						_, values = uploadTaskReadFixture()
					}
					return &uploadTaskReadRows{&idempotencyRecordRows{values: values}}, nil
				}
				<-queryContext.Done()
				return nil, queryContext.Err()
			}}}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := store.FindUserUploadTask(ctx, 7, 19)
			wantError := error(context.DeadlineExceeded)
			if strings.HasPrefix(phase, "after") {
				wantError = context.Canceled
			}
			if actual != nil || !errors.Is(err, wantError) {
				t.Fatal("上传任务取消后返回了结果或丢失原因", err)
			}
		})
	}
}

func TestFindUserUploadTaskCopies(t *testing.T) {
	// 1. 连续读取同一组来源，比较每个可空字段的独立指针。
	want, values := uploadTaskReadFixture()
	connector := &deviceConnector{accountConnector: accountConnector{query: func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
		return &uploadTaskReadRows{&idempotencyRecordRows{values: values}}, nil
	}}}
	pool := sql.OpenDB(connector)
	defer pool.Close()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.FindUserUploadTask(context.Background(), 7, 19)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.FindUserUploadTask(context.Background(), 7, 19)
	if err != nil {
		t.Fatal(err)
	}
	firstValue, secondValue := reflect.ValueOf(first).Elem(), reflect.ValueOf(second).Elem()
	for index := range firstValue.NumField() {
		field := firstValue.Field(index)
		if field.Kind() == reflect.Pointer && !field.IsNil() {
			if field.Pointer() == secondValue.Field(index).Pointer() {
				t.Fatal("两次读取共享可空字段")
			}
			field.Elem().Set(reflect.Zero(field.Type().Elem()))
		}
	}
	// 2. 修改来源二进制字节，另一份完整结果仍持有原值。
	values[10].([]byte)[0] ^= 0xff
	if !reflect.DeepEqual(second, &want) {
		t.Fatal("修改来源或另一结果影响了已交付任务")
	}
}
