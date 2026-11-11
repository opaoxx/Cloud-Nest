package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type userProfileRows struct {
	values []driver.Value
	read   bool
}

func (*userProfileRows) Columns() []string {
	return []string{"id", "username", "display_name", "status", "root_entry_id", "quota_bytes", "used_bytes", "reserved_bytes"}
}
func (*userProfileRows) Close() error { return nil }
func (rows *userProfileRows) Next(destination []driver.Value) error {
	if rows.read || rows.values == nil {
		return io.EOF
	}
	copy(destination, rows.values)
	rows.read = true
	return nil
}

func TestFindUserProfileResults(t *testing.T) {
	base := domain.UserProfile{ID: 7, Username: "fixture", DisplayName: "学习用户 🌱", Status: "ACTIVE", RootEntryID: 13, QuotaBytes: 100, UsedBytes: 60, ReservedBytes: 40}
	disabled, emptyName, zeroQuota, maxUsed, maxReserved, unknown := base, base, base, base, base, base
	disabled.Status, emptyName.DisplayName, unknown.Status = "DISABLED", "", "UNKNOWN"
	zeroQuota.QuotaBytes, zeroQuota.UsedBytes, zeroQuota.ReservedBytes = 0, 0, 0
	maxUsed.ID, maxUsed.RootEntryID, maxUsed.QuotaBytes, maxUsed.UsedBytes, maxUsed.ReservedBytes = ^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0), 0
	maxReserved.QuotaBytes, maxReserved.UsedBytes, maxReserved.ReservedBytes = ^uint64(0), 0, ^uint64(0)
	fault := errors.New("fixture query failure")
	for _, testCase := range []struct {
		name     string
		userID   uint64
		values   []driver.Value
		queryErr error
		want     *domain.UserProfile
		wantErr  error
	}{
		{name: "active and full quota", userID: 7, values: []driver.Value{int64(7), "fixture", "学习用户 🌱", "ACTIVE", int64(13), int64(100), int64(60), int64(40)}, want: &base},
		{name: "disabled preserved", userID: 7, values: []driver.Value{int64(7), "fixture", "学习用户 🌱", "DISABLED", int64(13), int64(100), int64(60), int64(40)}, want: &disabled},
		{name: "empty display name preserved", userID: 7, values: []driver.Value{int64(7), "fixture", "", "ACTIVE", int64(13), int64(100), int64(60), int64(40)}, want: &emptyName},
		{name: "zero quota", userID: 7, values: []driver.Value{int64(7), "fixture", "学习用户 🌱", "ACTIVE", int64(13), int64(0), int64(0), int64(0)}, want: &zeroQuota},
		{name: "maximum ids and usage", userID: ^uint64(0), values: []driver.Value{"18446744073709551615", "fixture", "学习用户 🌱", "ACTIVE", "18446744073709551615", "18446744073709551615", "18446744073709551615", int64(0)}, want: &maxUsed},
		{name: "maximum reservation", userID: 7, values: []driver.Value{int64(7), "fixture", "学习用户 🌱", "ACTIVE", int64(13), "18446744073709551615", int64(0), "18446744073709551615"}, want: &maxReserved},
		{name: "unknown status preserved", userID: 7, values: []driver.Value{int64(7), "fixture", "学习用户 🌱", "UNKNOWN", int64(13), int64(100), int64(60), int64(40)}, want: &unknown},
		{name: "not found", userID: 7, wantErr: domain.ErrAccountNotFound},
		{name: "missing root", userID: 7, values: []driver.Value{int64(7), "fixture", "学习用户 🌱", "ACTIVE", nil, int64(100), int64(60), int64(40)}, wantErr: domain.ErrInvalidAccountData},
		{name: "zero root", userID: 7, values: []driver.Value{int64(7), "fixture", "学习用户 🌱", "ACTIVE", int64(0), int64(100), int64(60), int64(40)}, wantErr: domain.ErrInvalidAccountData},
		{name: "wrong owner", userID: 7, values: []driver.Value{int64(8), "fixture", "学习用户 🌱", "ACTIVE", int64(13), int64(100), int64(60), int64(40)}, wantErr: domain.ErrInvalidAccountData},
		{name: "usage above quota", userID: 7, values: []driver.Value{int64(7), "fixture", "学习用户 🌱", "ACTIVE", int64(13), int64(100), int64(101), int64(0)}, wantErr: domain.ErrInvalidAccountData},
		{name: "reservation above remaining quota", userID: 7, values: []driver.Value{int64(7), "fixture", "学习用户 🌱", "ACTIVE", int64(13), int64(100), int64(60), int64(41)}, wantErr: domain.ErrInvalidAccountData},
		{name: "overflowing sum", userID: 7, values: []driver.Value{int64(7), "fixture", "学习用户 🌱", "ACTIVE", int64(13), "18446744073709551615", "18446744073709551615", int64(1)}, wantErr: domain.ErrInvalidAccountData},
		{name: "scan invalid id", userID: 7, values: []driver.Value{"bad-id", "fixture", "学习用户 🌱", "ACTIVE", int64(13), int64(100), int64(60), int64(40)}},
		{name: "scan null quota", userID: 7, values: []driver.Value{int64(7), "fixture", "学习用户 🌱", "ACTIVE", int64(13), nil, int64(60), int64(40)}},
		{name: "scan negative quota", userID: 7, values: []driver.Value{int64(7), "fixture", "学习用户 🌱", "ACTIVE", int64(13), int64(-1), int64(0), int64(0)}},
		{name: "query failure", userID: 7, queryErr: fault, wantErr: fault},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			queries := 0
			connector := &deviceConnector{accountConnector: accountConnector{query: func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				queries++
				expected := "SELECT accounts.id AS id,accounts.username AS username,accounts.display_name AS display_name,accounts.status AS status,roots.id AS root_entry_id,accounts.storage_quota AS quota_bytes,accounts.used_storage AS used_bytes,accounts.reserved_storage AS reserved_bytes FROM users AS accounts LEFT JOIN file_entries AS roots ON roots.root_owner_id = accounts.id AND roots.user_id = accounts.id AND roots.parent_id IS NULL AND roots.entry_type = ? AND roots.status = ? WHERE accounts.id = ? LIMIT ?"
				actual := strings.Join(strings.Fields(strings.ReplaceAll(statement, "`", "")), " ")
				if actual != expected || len(arguments) != 4 || arguments[0].Value != "DIR" || arguments[1].Value != "ACTIVE" || arguments[2].Value != testCase.userID || arguments[3].Value != int64(1) {
					t.Error("用户资料查询列清单、根目录归属或绑定参数错误")
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) > 5*time.Second {
					t.Error("查询缺少期限")
				}
				return &userProfileRows{values: testCase.values}, testCase.queryErr
			}}}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := store.FindUserProfile(context.Background(), testCase.userID)
			if queries != 1 {
				t.Fatal("用户资料查询未恰好执行一次")
			}
			if testCase.want != nil {
				if err != nil || !reflect.DeepEqual(actual, testCase.want) {
					t.Fatalf("用户资料字段未完整读回: %v", err)
				}
			} else if err == nil || actual != nil || testCase.wantErr != nil && !errors.Is(err, testCase.wantErr) {
				t.Fatalf("未找到或异常结果错误: %v", err)
			}
			if err := pool.PingContext(context.Background()); err != nil {
				t.Fatal("查询关闭了共享连接池")
			}
		})
	}
}

func TestFindUserProfileInput(t *testing.T) {
	connector := &deviceConnector{}
	pool := sql.OpenDB(connector)
	defer pool.Close()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, testCase := range []struct {
		ctx    context.Context
		userID uint64
		want   error
	}{
		{userID: 7},
		{ctx: context.Background()},
		{ctx: canceled, userID: 7, want: context.Canceled},
	} {
		result, err := store.FindUserProfile(testCase.ctx, testCase.userID)
		if result != nil || err == nil || testCase.want != nil && !errors.Is(err, testCase.want) || connector.connections.Load() != 0 {
			t.Error("无效输入或预先取消仍访问了数据库")
		}
	}
}

func TestFindUserProfileCancellation(t *testing.T) {
	for _, phase := range []string{"caller deadline", "default deadline", "after result"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if phase == "caller deadline" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 50*time.Millisecond)
				defer stop()
			}
			connector := &deviceConnector{accountConnector: accountConnector{query: func(queryContext context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
				if phase == "after result" {
					cancel()
					return &userProfileRows{values: []driver.Value{int64(7), "fixture", "fixture", "ACTIVE", int64(13), int64(0), int64(0), int64(0)}}, nil
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
			result, err := store.FindUserProfile(ctx, 7)
			want := error(context.DeadlineExceeded)
			if phase == "after result" {
				want = context.Canceled
			}
			if result != nil || !errors.Is(err, want) {
				t.Fatal("取消后返回了资料或丢失取消原因")
			}
		})
	}
}
