package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
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

type refreshSessionRows struct {
	values []driver.Value
	read   bool
}

func (*refreshSessionRows) Columns() []string {
	return []string{"user_id", "session_id", "device_id", "user_status", "session_expires_at", "session_revoked_at", "device_revoked_at", "token_generation"}
}
func (*refreshSessionRows) Close() error { return nil }
func (rows *refreshSessionRows) Next(destination []driver.Value) error {
	if rows.read || rows.values == nil {
		return io.EOF
	}
	copy(destination, rows.values)
	rows.read = true
	return nil
}

func TestFindRefreshSessionResults(t *testing.T) {
	expiresAt := time.Date(2026, 12, 12, 8, 0, 0, 123456000, time.UTC)
	revokedAt := expiresAt.Add(-time.Hour)
	base := domain.RefreshSession{SessionAuthorization: domain.SessionAuthorization{UserID: 7, SessionID: 11, DeviceID: 13, UserStatus: "ACTIVE", SessionExpiresAt: expiresAt}, TokenGeneration: 1}
	disabled, unknown, revokedSession, revokedDevice, expired, nextGeneration, large := base, base, base, base, base, base, base
	disabled.UserStatus, unknown.UserStatus = "DISABLED", "UNKNOWN"
	revokedSession.SessionRevokedAt, revokedDevice.DeviceRevokedAt = &revokedAt, &revokedAt
	expired.SessionExpiresAt = expiresAt.Add(-365 * 24 * time.Hour)
	nextGeneration.TokenGeneration = 42
	large.UserID, large.SessionID, large.DeviceID, large.TokenGeneration = ^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0)
	fault := errors.New("fixture database failure")
	for _, testCase := range []struct {
		name     string
		values   []driver.Value
		queryErr error
		want     *domain.RefreshSession
		wantErr  error
		zeroHash bool
	}{
		{name: "active null revocations", values: []driver.Value{int64(7), int64(11), int64(13), "ACTIVE", expiresAt, nil, nil, int64(1)}, want: &base},
		{name: "disabled preserved", values: []driver.Value{int64(7), int64(11), int64(13), "DISABLED", expiresAt, nil, nil, int64(1)}, want: &disabled},
		{name: "unknown status preserved", values: []driver.Value{int64(7), int64(11), int64(13), "UNKNOWN", expiresAt, nil, nil, int64(1)}, want: &unknown},
		{name: "revoked session preserved", values: []driver.Value{int64(7), int64(11), int64(13), "ACTIVE", expiresAt, revokedAt, nil, int64(1)}, want: &revokedSession},
		{name: "revoked device preserved", values: []driver.Value{int64(7), int64(11), int64(13), "ACTIVE", expiresAt, nil, revokedAt, int64(1)}, want: &revokedDevice},
		{name: "expired preserved", values: []driver.Value{int64(7), int64(11), int64(13), "ACTIVE", expired.SessionExpiresAt, nil, nil, int64(1)}, want: &expired},
		{name: "generation preserved", values: []driver.Value{int64(7), int64(11), int64(13), "ACTIVE", expiresAt, nil, nil, int64(42)}, want: &nextGeneration},
		{name: "maximum unsigned integers", values: []driver.Value{"18446744073709551615", "18446744073709551615", "18446744073709551615", "ACTIVE", expiresAt, nil, nil, "18446744073709551615"}, want: &large},
		{name: "not found", wantErr: domain.ErrSessionNotFound},
		{name: "query error", queryErr: fault, wantErr: fault},
		{name: "scan error", values: []driver.Value{int64(7), int64(11), int64(13), "ACTIVE", expiresAt, nil, nil, "invalid-generation"}},
		{name: "zero user", values: []driver.Value{int64(0), int64(11), int64(13), "ACTIVE", expiresAt, nil, nil, int64(1)}, wantErr: domain.ErrInvalidSessionData},
		{name: "zero session", values: []driver.Value{int64(7), int64(0), int64(13), "ACTIVE", expiresAt, nil, nil, int64(1)}, wantErr: domain.ErrInvalidSessionData},
		{name: "zero device", values: []driver.Value{int64(7), int64(11), int64(0), "ACTIVE", expiresAt, nil, nil, int64(1)}, wantErr: domain.ErrInvalidSessionData},
		{name: "zero generation", values: []driver.Value{int64(7), int64(11), int64(13), "ACTIVE", expiresAt, nil, nil, int64(0)}, wantErr: domain.ErrInvalidSessionData},
		{name: "all zero digest is a full length value", zeroHash: true, values: []driver.Value{int64(7), int64(11), int64(13), "ACTIVE", expiresAt, nil, nil, int64(1)}, want: &base},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			hash := sha256.Sum256([]byte("fixture-refresh-token"))
			if testCase.zeroHash {
				hash = [sha256.Size]byte{}
			}
			calls := 0
			connector := &deviceConnector{accountConnector: accountConnector{query: func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				calls++
				expected := "SELECT sessions.user_id AS user_id,sessions.id AS session_id,sessions.device_id AS device_id,accounts.status AS user_status,sessions.expires_at AS session_expires_at,sessions.revoked_at AS session_revoked_at,devices.revoked_at AS device_revoked_at,sessions.token_generation AS token_generation FROM auth_sessions AS sessions JOIN users AS accounts ON accounts.id = sessions.user_id JOIN user_devices AS devices ON devices.user_id = sessions.user_id AND devices.id = sessions.device_id WHERE sessions.refresh_token_hash = ? LIMIT ?"
				actual := strings.Join(strings.Fields(strings.ReplaceAll(statement, "`", "")), " ")
				if actual != expected || len(arguments) != 2 {
					t.Fatal("刷新查询字段、关联或参数数量错误")
				}
				bound, valid := arguments[0].Value.([]byte)
				if !valid || !bytes.Equal(bound, hash[:]) || arguments[1].Value != int64(1) {
					t.Error("摘要未作为完整二进制值绑定")
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) > 5*time.Second {
					t.Error("查询缺少期限")
				}
				return &refreshSessionRows{values: testCase.values}, testCase.queryErr
			}}}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			result, err := store.FindRefreshSession(context.Background(), hash)
			if calls != 1 {
				t.Fatal("刷新会话定位未恰好执行一次查询")
			}
			if testCase.want != nil {
				if err != nil || !reflect.DeepEqual(result, testCase.want) {
					t.Fatalf("刷新字段或 NULL 映射错误: %v", err)
				}
			} else if result != nil || err == nil || testCase.wantErr != nil && !errors.Is(err, testCase.wantErr) {
				t.Fatalf("未找到或故障结果错误: %v", err)
			}
			if err := pool.PingContext(context.Background()); err != nil {
				t.Fatal("查询关闭了共享连接池")
			}
		})
	}
}

func TestFindRefreshSessionInputAndCancellation(t *testing.T) {
	for _, phase := range []string{"nil context", "already canceled", "caller deadline", "default deadline", "after result"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if phase == "nil context" {
				ctx = nil
			}
			if phase == "already canceled" {
				cancel()
			}
			if phase == "caller deadline" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 50*time.Millisecond)
				defer stop()
			}
			connector := &deviceConnector{accountConnector: accountConnector{query: func(queryContext context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
				if phase == "after result" {
					cancel()
					return &refreshSessionRows{values: []driver.Value{int64(7), int64(11), int64(13), "ACTIVE", time.Now().UTC(), nil, nil, int64(1)}}, nil
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
			result, err := store.FindRefreshSession(ctx, sha256.Sum256([]byte("fixture")))
			if result != nil || err == nil {
				t.Fatal("错误或取消返回了会话结果")
			}
			if phase == "nil context" || phase == "already canceled" {
				if connector.connections.Load() != 0 {
					t.Fatal("无效上下文仍连接了数据库")
				}
			}
			if phase == "already canceled" || phase == "after result" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal("取消原因未保留")
				}
			}
			if phase == "caller deadline" || phase == "default deadline" {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("期限原因未保留")
				}
			}
		})
	}
}
