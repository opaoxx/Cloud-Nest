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

type sessionAuthorizationRows struct {
	values []driver.Value
	read   bool
}

func (*sessionAuthorizationRows) Columns() []string {
	return []string{"user_id", "session_id", "device_id", "user_status", "session_expires_at", "session_revoked_at", "device_revoked_at"}
}
func (*sessionAuthorizationRows) Close() error { return nil }
func (rows *sessionAuthorizationRows) Next(destination []driver.Value) error {
	if rows.read || rows.values == nil {
		return io.EOF
	}
	copy(destination, rows.values)
	rows.read = true
	return nil
}

func TestFindSessionAuthorizationResults(t *testing.T) {
	expiresAt := time.Date(2026, 11, 30, 3, 0, 0, 123456000, time.UTC)
	revokedAt := time.Date(2026, 11, 11, 2, 0, 0, 654321000, time.UTC)
	base := domain.SessionAuthorization{UserID: 7, SessionID: 11, DeviceID: 13, UserStatus: "ACTIVE", SessionExpiresAt: expiresAt}
	disabled, revokedSession, revokedDevice, expired, large := base, base, base, base, base
	disabled.UserStatus = "DISABLED"
	revokedSession.SessionRevokedAt = &revokedAt
	revokedDevice.DeviceRevokedAt = &revokedAt
	expired.SessionExpiresAt = expiresAt.Add(-365 * 24 * time.Hour)
	large.UserID, large.SessionID, large.DeviceID = ^uint64(0), ^uint64(0), ^uint64(0)
	queryFailure := errors.New("fixture query failure")
	for _, testCase := range []struct {
		name      string
		userID    uint64
		sessionID uint64
		values    []driver.Value
		queryErr  error
		want      *domain.SessionAuthorization
		wantErr   error
	}{
		{name: "active null times", userID: 7, sessionID: 11, values: []driver.Value{int64(7), int64(11), int64(13), "ACTIVE", expiresAt, nil, nil}, want: &base},
		{name: "disabled preserved", userID: 7, sessionID: 11, values: []driver.Value{int64(7), int64(11), int64(13), "DISABLED", expiresAt, nil, nil}, want: &disabled},
		{name: "revoked session preserved", userID: 7, sessionID: 11, values: []driver.Value{int64(7), int64(11), int64(13), "ACTIVE", expiresAt, revokedAt, nil}, want: &revokedSession},
		{name: "revoked device preserved", userID: 7, sessionID: 11, values: []driver.Value{int64(7), int64(11), int64(13), "ACTIVE", expiresAt, nil, revokedAt}, want: &revokedDevice},
		{name: "expired preserved", userID: 7, sessionID: 11, values: []driver.Value{int64(7), int64(11), int64(13), "ACTIVE", expired.SessionExpiresAt, nil, nil}, want: &expired},
		{name: "unsigned ids", userID: ^uint64(0), sessionID: ^uint64(0), values: []driver.Value{"18446744073709551615", "18446744073709551615", "18446744073709551615", "ACTIVE", expiresAt, nil, nil}, want: &large},
		{name: "not found", userID: 7, sessionID: 11, wantErr: domain.ErrSessionNotFound},
		{name: "query failure", userID: 7, sessionID: 11, queryErr: queryFailure, wantErr: queryFailure},
		{name: "scan failure", userID: 7, sessionID: 11, values: []driver.Value{int64(7), "bad-id", int64(13), "ACTIVE", expiresAt, nil, nil}},
		{name: "bad timestamp", userID: 7, sessionID: 11, values: []driver.Value{int64(7), int64(11), int64(13), "ACTIVE", "bad-time", nil, nil}},
		{name: "wrong owner", userID: 7, sessionID: 11, values: []driver.Value{int64(8), int64(11), int64(13), "ACTIVE", expiresAt, nil, nil}, wantErr: domain.ErrInvalidSessionData},
		{name: "wrong session", userID: 7, sessionID: 11, values: []driver.Value{int64(7), int64(12), int64(13), "ACTIVE", expiresAt, nil, nil}, wantErr: domain.ErrInvalidSessionData},
		{name: "zero device", userID: 7, sessionID: 11, values: []driver.Value{int64(7), int64(11), int64(0), "ACTIVE", expiresAt, nil, nil}, wantErr: domain.ErrInvalidSessionData},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			queryCount := 0
			connector := &deviceConnector{accountConnector: accountConnector{query: func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				queryCount++
				// 绑定两项身份，设备通过所属用户与设备 ID 两个条件关联。
				const expected = "SELECT sessions.user_id AS user_id,sessions.id AS session_id,sessions.device_id AS device_id,accounts.status AS user_status,sessions.expires_at AS session_expires_at,sessions.revoked_at AS session_revoked_at,devices.revoked_at AS device_revoked_at FROM auth_sessions AS sessions JOIN users AS accounts ON accounts.id = sessions.user_id JOIN user_devices AS devices ON devices.user_id = sessions.user_id AND devices.id = sessions.device_id WHERE sessions.user_id = ? AND sessions.id = ? LIMIT ?"
				actual := strings.Join(strings.Fields(strings.ReplaceAll(statement, "`", "")), " ")
				if actual != expected || len(arguments) != 3 || arguments[0].Value != testCase.userID || arguments[1].Value != testCase.sessionID || arguments[2].Value != int64(1) {
					t.Errorf("授权查询的关联、列清单或参数错误: %s", actual)
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) > 5*time.Second {
					t.Error("查询缺少期限")
				}
				return &sessionAuthorizationRows{values: testCase.values}, testCase.queryErr
			}}}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := store.FindSessionAuthorization(context.Background(), testCase.userID, testCase.sessionID)
			if queryCount != 1 {
				t.Fatal("本次调用没有恰好执行一条查询")
			}
			if testCase.want != nil {
				if err != nil || !reflect.DeepEqual(actual, testCase.want) {
					t.Fatalf("授权字段、NULL 或时间映射不一致: %v", err)
				}
			} else if actual != nil || err == nil || (testCase.wantErr != nil && !errors.Is(err, testCase.wantErr)) {
				t.Fatalf("未命中或故障结果错误: %v", err)
			}
			if err := pool.PingContext(context.Background()); err != nil {
				t.Fatal("查询关闭了共享连接池")
			}
		})
	}
}

func TestFindSessionAuthorizationInput(t *testing.T) {
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
		ctx               context.Context
		userID, sessionID uint64
		want              error
	}{
		{userID: 7, sessionID: 11},
		{ctx: context.Background(), sessionID: 11},
		{ctx: context.Background(), userID: 7},
		{ctx: canceled, userID: 7, sessionID: 11, want: context.Canceled},
	} {
		actual, err := store.FindSessionAuthorization(testCase.ctx, testCase.userID, testCase.sessionID)
		if actual != nil || err == nil || (testCase.want != nil && !errors.Is(err, testCase.want)) || connector.connections.Load() != 0 {
			t.Error("无效输入或预先取消仍访问了数据库")
		}
	}
}

func TestFindSessionAuthorizationCancellation(t *testing.T) {
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
					return &sessionAuthorizationRows{values: []driver.Value{int64(7), int64(11), int64(13), "ACTIVE", time.Now().UTC(), nil, nil}}, nil
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
			actual, err := store.FindSessionAuthorization(ctx, 7, 11)
			want := error(context.DeadlineExceeded)
			if phase == "after result" {
				want = context.Canceled
			}
			if actual != nil || !errors.Is(err, want) {
				t.Fatal("查询取消后返回状态或丢失取消原因")
			}
		})
	}
}
