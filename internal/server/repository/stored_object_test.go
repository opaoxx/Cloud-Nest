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

// storedObjectReadRows 复用行错误处理和既有对象列清单。
type storedObjectReadRows struct{ *idempotencyRecordRows }

func (*storedObjectReadRows) Columns() []string { return (&storedObjectMappingRows{}).Columns() }

// storedObjectReadFixture 提供完整 READY 对象及独立时间事实。
func storedObjectReadFixture() (domain.StoredObject, []driver.Value) {
	stamp := time.Date(2026, 11, 17, 8, 0, 0, 123456000, time.UTC)
	versionID, storageETag, errorCode := "version-1", `"opaque-etag-1"`, "STORAGE_RETRY"
	verifiedAt, unreferencedAt := stamp.Add(time.Minute), stamp.Add(2*time.Minute)
	gcAfter := unreferencedAt.Add(7 * 24 * time.Hour)
	digest := sha256.Sum256([]byte("stored object fixture"))
	dedupHash := digest
	want := domain.StoredObject{
		ID: 23, OwnerUserID: 7, Provider: "MINIO", Bucket: "fixture-bucket", ObjectKey: "objects/fixture",
		StorageVersionID: &versionID, FileSize: 2048, VerifiedSHA256: &digest,
		Status: domain.StoredObjectStatusReady, DedupHash: &dedupHash,
		StorageETag: &storageETag, VerifiedAt: &verifiedAt, UnreferencedAt: &unreferencedAt,
		GCAfter: &gcAfter, LastErrorCode: &errorCode, CreatedAt: stamp, UpdatedAt: stamp.Add(time.Second),
	}
	values := []driver.Value{
		int64(23), int64(7), want.Provider, want.Bucket, want.ObjectKey, versionID,
		int64(2048), append([]byte(nil), digest[:]...), "READY", append([]byte(nil), digest[:]...),
		storageETag, verifiedAt, unreferencedAt, gcAfter, errorCode, want.CreatedAt, want.UpdatedAt,
	}
	return want, values
}

func TestFindUserStoredObjectResults(t *testing.T) {
	modes := []string{
		"full record", "nullable staging", "zero digests", "maximum unsigned", "UTC conversion",
		"time boundaries", "equal GC times", "explicit empty optional text", "missing", "query failure", "row failure",
	}
	for _, status := range []string{"STAGING", "VERIFYING", "READY", "ORPHANED", "DELETING", "DELETED"} {
		modes = append(modes, status)
	}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			// 1. 准备合法对象及本组空值、状态或故障。
			want, values := storedObjectReadFixture()
			fault := errors.New("private object query failure")
			var queryError, rowsError, wantError error
			switch mode {
			case "nullable staging":
				want = domain.StoredObject{ID: 23, OwnerUserID: 7, Provider: want.Provider, Bucket: want.Bucket,
					ObjectKey: want.ObjectKey, Status: domain.StoredObjectStatusStaging, CreatedAt: want.CreatedAt, UpdatedAt: want.UpdatedAt}
				for _, column := range []int{5, 7, 9, 10, 11, 12, 13, 14} {
					values[column] = nil
				}
				values[6], values[8] = int64(0), "STAGING"
			case "zero digests":
				want.VerifiedSHA256, want.DedupHash = new([sha256.Size]byte), new([sha256.Size]byte)
				values[7], values[9] = make([]byte, sha256.Size), make([]byte, sha256.Size)
			case "maximum unsigned":
				want.ID, want.OwnerUserID, want.FileSize = ^uint64(0), ^uint64(0), ^uint64(0)
				for _, column := range []int{0, 1, 6} {
					values[column] = "18446744073709551615"
				}
			case "UTC conversion":
				zone := time.FixedZone("fixture", 8*60*60)
				for _, column := range []int{11, 12, 13, 15, 16} {
					values[column] = values[column].(time.Time).In(zone)
				}
			case "time boundaries":
				want.CreatedAt = time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
				want.UpdatedAt = time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
				values[15], values[16] = want.CreatedAt, want.UpdatedAt
			case "equal GC times":
				*want.GCAfter = *want.UnreferencedAt
				values[13] = *want.GCAfter
			case "explicit empty optional text":
				*want.StorageVersionID, *want.StorageETag, *want.LastErrorCode = "", "", ""
				values[5], values[10], values[14] = "", "", ""
			case "missing":
				values, wantError = nil, domain.ErrStoredObjectNotFound
			case "query failure":
				queryError, wantError = fault, fault
			case "row failure":
				rowsError, wantError = fault, fault
			default:
				if domain.StoredObjectStatus(mode).IsValid() {
					want.Status, values[8] = domain.StoredObjectStatus(mode), mode
					if want.Status != domain.StoredObjectStatusReady {
						want.DedupHash, values[9] = nil, nil
					}
				}
			}
			// 2. 核对显式投影、用户/对象参数及五秒内期限。
			queries := 0
			connector := &deviceConnector{accountConnector: accountConnector{query: func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				queries++
				expectedSQL := "SELECT " + strings.Join((&storedObjectMappingRows{}).Columns(), ",") + " FROM stored_objects WHERE owner_user_id = ? AND id = ? LIMIT ?"
				actualSQL := strings.Join(strings.Fields(strings.ReplaceAll(statement, "`", "")), " ")
				if actualSQL != expectedSQL || len(arguments) != 3 || arguments[0].Value != want.OwnerUserID || arguments[1].Value != want.ID || arguments[2].Value != int64(1) {
					t.Error("存储对象投影或范围参数错误")
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
					t.Error("存储对象查询期限错误")
				}
				return &storedObjectReadRows{&idempotencyRecordRows{values: values, err: rowsError}}, queryError
			}}}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			// 3. 检查独立结果、固定缺失错误或可追溯故障。
			actual, err := store.FindUserStoredObject(context.Background(), want.OwnerUserID, want.ID)
			if wantError != nil {
				if actual != nil || !errors.Is(err, wantError) {
					t.Fatal("存储对象缺失或故障处理错误", err)
				}
			} else if err != nil || !reflect.DeepEqual(actual, &want) {
				t.Fatal("存储对象事实未完整读回", err)
			}
			if queries != 1 || pool.PingContext(context.Background()) != nil {
				t.Fatal("存储对象查询次数或共享池生命周期错误")
			}
		})
	}
}

func TestFindUserStoredObjectInvalidRecords(t *testing.T) {
	fixtures := []struct {
		name    string
		changes map[int]driver.Value
	}{
		{"wrong id", map[int]driver.Value{0: int64(24)}},
		{"wrong owner", map[int]driver.Value{1: int64(8)}},
		{"empty provider", map[int]driver.Value{2: ""}},
		{"empty bucket", map[int]driver.Value{3: ""}},
		{"empty object key", map[int]driver.Value{4: ""}},
		{"non-ASCII provider", map[int]driver.Value{2: "存储"}},
		{"unknown status", map[int]driver.Value{8: "UNKNOWN"}},
		{"lowercase status", map[int]driver.Value{8: "ready"}},
		{"READY missing digest", map[int]driver.Value{7: nil}},
		{"READY missing verification time", map[int]driver.Value{11: nil}},
		{"READY missing dedup hash", map[int]driver.Value{9: nil}},
		{"READY mismatched dedup hash", map[int]driver.Value{9: make([]byte, 32)}},
		{"non-READY dedup hash", map[int]driver.Value{8: "ORPHANED"}},
		{"missing GC start", map[int]driver.Value{12: nil}},
		{"missing GC deadline", map[int]driver.Value{13: nil}},
		{"reversed GC time", map[int]driver.Value{13: time.Date(2026, 11, 17, 8, 0, 0, 123456000, time.UTC)}},
	}
	// 1. 分别覆盖全部 ASCII 文本、摘要长度及五个时间字段。
	for _, column := range []int{2, 3, 4, 5, 10, 14} {
		fixtures = append(fixtures, struct {
			name    string
			changes map[int]driver.Value
		}{"invalid ASCII " + (&storedObjectMappingRows{}).Columns()[column], map[int]driver.Value{column: "\xff"}})
	}
	for _, column := range []int{7, 9} {
		for _, digest := range [][]byte{{}, make([]byte, 31), make([]byte, 33)} {
			fixtures = append(fixtures, struct {
				name    string
				changes map[int]driver.Value
			}{"invalid digest " + (&storedObjectMappingRows{}).Columns()[column], map[int]driver.Value{column: digest}})
		}
	}
	for _, column := range []int{11, 12, 13, 15, 16} {
		for _, stamp := range []time.Time{
			{}, time.Date(999, 12, 31, 23, 59, 59, 0, time.UTC),
			time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 17, 8, 0, 0, 1, time.UTC),
		} {
			fixtures = append(fixtures, struct {
				name    string
				changes map[int]driver.Value
			}{"invalid time " + (&storedObjectMappingRows{}).Columns()[column], map[int]driver.Value{column: stamp}})
		}
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			// 2. 异常记录整体返回 nil 及固定数据错误。
			_, values := storedObjectReadFixture()
			for column, value := range fixture.changes {
				values[column] = value
			}
			connector := &deviceConnector{accountConnector: accountConnector{query: func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
				return &storedObjectReadRows{&idempotencyRecordRows{values: values}}, nil
			}}}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := store.FindUserStoredObject(context.Background(), 7, 23)
			if actual != nil || !errors.Is(err, domain.ErrInvalidStoredObjectData) || err.Error() != "存储对象数据异常" {
				t.Fatal("异常存储对象被接受或错误提示泄露原值", err)
			}
		})
	}
}

func TestFindUserStoredObjectInput(t *testing.T) {
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
		ctx              context.Context
		userID, objectID uint64
		wantError        error
	}{
		{userID: 7, objectID: 19},
		{ctx: context.Background(), objectID: 19},
		{ctx: context.Background(), userID: 7},
		{ctx: cancelled, userID: 7, objectID: 19, wantError: context.Canceled},
		{ctx: expired, userID: 7, objectID: 19, wantError: context.DeadlineExceeded},
	} {
		actual, err := store.FindUserStoredObject(fixture.ctx, fixture.userID, fixture.objectID)
		if actual != nil || err == nil || (fixture.wantError != nil && !errors.Is(err, fixture.wantError)) || connector.connections.Load() != 0 {
			t.Fatal("无效调用访问数据库或丢失取消原因", err)
		}
	}
}

func TestFindUserStoredObjectCancellation(t *testing.T) {
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
						return nil, errors.New("private cancelled object query")
					}
					var values []driver.Value
					if phase == "after result" {
						_, values = storedObjectReadFixture()
					}
					return &storedObjectReadRows{&idempotencyRecordRows{values: values}}, nil
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
			actual, err := store.FindUserStoredObject(ctx, 7, 23)
			wantError := error(context.DeadlineExceeded)
			if strings.HasPrefix(phase, "after") {
				wantError = context.Canceled
			}
			if actual != nil || !errors.Is(err, wantError) {
				t.Fatal("存储对象取消后返回了结果或丢失原因", err)
			}
		})
	}
}

func TestFindUserStoredObjectCopies(t *testing.T) {
	// 1. 连续读取同一组来源，比较每个可空字段的独立指针。
	want, values := storedObjectReadFixture()
	connector := &deviceConnector{accountConnector: accountConnector{query: func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
		return &storedObjectReadRows{&idempotencyRecordRows{values: values}}, nil
	}}}
	pool := sql.OpenDB(connector)
	defer pool.Close()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.FindUserStoredObject(context.Background(), 7, 23)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.FindUserStoredObject(context.Background(), 7, 23)
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
	values[7].([]byte)[0] ^= 0xff
	values[9].([]byte)[0] ^= 0xff
	if !reflect.DeepEqual(second, &want) {
		t.Fatal("修改来源或另一结果影响了已交付任务")
	}
}
