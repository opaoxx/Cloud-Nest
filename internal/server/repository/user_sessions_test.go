package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// userSessionRows 提供分页查询的十列结果及读取中途故障。
type userSessionRows struct {
	records       [][]driver.Value
	position      int
	terminalError error
}

func (*userSessionRows) Columns() []string {
	return []string{"id", "user_id", "device_id", "device_name", "device_platform", "created_at", "expires_at", "last_seen_at", "revoked_at", "device_revoked_at"}
}

func (*userSessionRows) Close() error { return nil }

func (rows *userSessionRows) Next(destination []driver.Value) error {
	if rows.position == len(rows.records) {
		if rows.terminalError != nil {
			return rows.terminalError
		}
		return io.EOF
	}
	copy(destination, rows.records[rows.position])
	rows.position++
	return nil
}

// userSessionValues 将夹具按查询列顺序映射为 SQL 驱动返回值。
func userSessionValues(session domain.UserSession) []driver.Value {
	values := []driver.Value{
		strconv.FormatUint(session.ID, 10), strconv.FormatUint(session.UserID, 10), strconv.FormatUint(session.DeviceID, 10),
		session.DeviceName, session.DevicePlatform, session.CreatedAt, session.ExpiresAt, nil, nil, nil,
	}
	if session.LastSeenAt != nil {
		values[7] = *session.LastSeenAt
	}
	if session.RevokedAt != nil {
		values[8] = *session.RevokedAt
	}
	if session.DeviceRevokedAt != nil {
		values[9] = *session.DeviceRevokedAt
	}
	return values
}

func TestListUserSessionsResults(t *testing.T) {
	createdAt := time.Date(2026, 11, 12, 3, 0, 0, 123456000, time.UTC)
	seenAt, revokedAt := createdAt.Add(time.Minute), createdAt.Add(time.Hour)
	base := domain.UserSession{ID: 11, UserID: 7, DeviceID: 13, DeviceName: "学习设备 📁", DevicePlatform: "WEB", CreatedAt: createdAt, ExpiresAt: createdAt.Add(24 * time.Hour)}
	older, oldest, history, large, wrongOwner, zeroID, zeroDevice := base, base, base, base, base, base, base
	older.ID, oldest.ID = 10, 9
	history.LastSeenAt, history.RevokedAt, history.DeviceRevokedAt = &seenAt, &revokedAt, &revokedAt
	history.ExpiresAt = createdAt.Add(-time.Hour)
	large.ID, large.UserID, large.DeviceID = ^uint64(0), ^uint64(0), ^uint64(0)
	wrongOwner.UserID, zeroID.ID, zeroDevice.DeviceID = 8, 0, 0
	badIDValues, badTimeValues := userSessionValues(base), userSessionValues(base)
	badIDValues[0], badTimeValues[5] = "bad-id", "bad-time"
	queryFailure, rowFailure := errors.New("fixture query failure"), errors.New("fixture row failure")
	var fullRecords [][]driver.Value
	var fullSessions []domain.UserSession
	for offset := range 101 {
		session := base
		session.ID = uint64(101 - offset)
		fullRecords = append(fullRecords, userSessionValues(session))
		fullSessions = append(fullSessions, session)
	}
	for _, testCase := range []struct {
		name                 string
		userID, beforeID     uint64
		pageSize             int
		records              [][]driver.Value
		queryError, rowError error
		want                 *domain.UserSessionPage
		wantError            error
	}{
		{name: "empty initialized page", userID: 7, pageSize: 2, want: &domain.UserSessionPage{Sessions: []domain.UserSession{}}},
		{name: "short page and null times", userID: 7, pageSize: 2, records: [][]driver.Value{userSessionValues(base)}, want: &domain.UserSessionPage{Sessions: []domain.UserSession{base}}},
		{name: "exact page", userID: 7, pageSize: 2, records: [][]driver.Value{userSessionValues(base), userSessionValues(older)}, want: &domain.UserSessionPage{Sessions: []domain.UserSession{base, older}}},
		{name: "extra row indicates more", userID: 7, pageSize: 2, records: [][]driver.Value{userSessionValues(base), userSessionValues(older), userSessionValues(oldest)}, want: &domain.UserSessionPage{Sessions: []domain.UserSession{base, older}, HasMore: true}},
		{name: "next page boundary", userID: 7, beforeID: 12, pageSize: 1, records: [][]driver.Value{userSessionValues(base), userSessionValues(older)}, want: &domain.UserSessionPage{Sessions: []domain.UserSession{base}, HasMore: true}},
		{name: "history states preserved", userID: 7, pageSize: 2, records: [][]driver.Value{userSessionValues(history)}, want: &domain.UserSessionPage{Sessions: []domain.UserSession{history}}},
		{name: "maximum page size", userID: 7, pageSize: 100, records: fullRecords, want: &domain.UserSessionPage{Sessions: fullSessions[:100], HasMore: true}},
		{name: "unsigned identifiers", userID: ^uint64(0), pageSize: 1, records: [][]driver.Value{userSessionValues(large)}, want: &domain.UserSessionPage{Sessions: []domain.UserSession{large}}},
		{name: "unsigned page boundary", userID: 7, beforeID: ^uint64(0), pageSize: 2, records: [][]driver.Value{userSessionValues(base)}, want: &domain.UserSessionPage{Sessions: []domain.UserSession{base}}},
		{name: "query failure", userID: 7, pageSize: 2, queryError: queryFailure, wantError: queryFailure},
		{name: "row failure after partial read", userID: 7, pageSize: 2, records: [][]driver.Value{userSessionValues(base)}, rowError: rowFailure, wantError: rowFailure},
		{name: "invalid identifier scan", userID: 7, pageSize: 2, records: [][]driver.Value{badIDValues}},
		{name: "invalid timestamp scan", userID: 7, pageSize: 2, records: [][]driver.Value{badTimeValues}},
		{name: "wrong owner", userID: 7, pageSize: 2, records: [][]driver.Value{userSessionValues(wrongOwner)}, wantError: domain.ErrInvalidSessionData},
		{name: "zero session", userID: 7, pageSize: 2, records: [][]driver.Value{userSessionValues(zeroID)}, wantError: domain.ErrInvalidSessionData},
		{name: "zero device", userID: 7, pageSize: 2, records: [][]driver.Value{userSessionValues(zeroDevice)}, wantError: domain.ErrInvalidSessionData},
		{name: "invalid later row discards page", userID: 7, pageSize: 2, records: [][]driver.Value{userSessionValues(base), userSessionValues(wrongOwner)}, wantError: domain.ErrInvalidSessionData},
		{name: "invalid extra row discards page", userID: 7, pageSize: 1, records: [][]driver.Value{userSessionValues(base), userSessionValues(wrongOwner)}, wantError: domain.ErrInvalidSessionData},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			queries := 0
			connector := &deviceConnector{accountConnector: accountConnector{query: func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				queries++
				// 显式列清单与双字段归属关联保证本次查询范围。
				expected := "SELECT sessions.id AS id,sessions.user_id AS user_id,sessions.device_id AS device_id,devices.name AS device_name,devices.platform AS device_platform,sessions.created_at AS created_at,sessions.expires_at AS expires_at,sessions.last_seen_at AS last_seen_at,sessions.revoked_at AS revoked_at,devices.revoked_at AS device_revoked_at FROM auth_sessions AS sessions JOIN user_devices AS devices ON devices.user_id = sessions.user_id AND devices.id = sessions.device_id WHERE sessions.user_id = ?"
				wantedArguments := []driver.NamedValue{{Ordinal: 1, Value: testCase.userID}}
				if testCase.beforeID != 0 {
					expected += " AND sessions.id < ?"
					wantedArguments = append(wantedArguments, driver.NamedValue{Ordinal: 2, Value: testCase.beforeID})
				}
				expected += " ORDER BY sessions.id DESC LIMIT ?"
				wantedArguments = append(wantedArguments, driver.NamedValue{Ordinal: len(wantedArguments) + 1, Value: int64(testCase.pageSize + 1)})
				actual := strings.Join(strings.Fields(strings.ReplaceAll(statement, "`", "")), " ")
				if actual != expected || !reflect.DeepEqual(arguments, wantedArguments) {
					t.Error("列表查询的列、归属、排序、页大小或绑定参数错误", actual)
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) > 5*time.Second {
					t.Error("列表查询缺少期限")
				}
				return &userSessionRows{records: testCase.records, terminalError: testCase.rowError}, testCase.queryError
			}}}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			page, err := store.ListUserSessions(context.Background(), testCase.userID, testCase.beforeID, testCase.pageSize)
			if queries != 1 {
				t.Fatal("本次列表查询未恰好执行一次")
			}
			if testCase.want != nil {
				if err != nil || !reflect.DeepEqual(page, testCase.want) {
					t.Fatal("分页、字段或 NULL 映射不正确", err)
				}
			} else if page != nil || err == nil || (testCase.wantError != nil && !errors.Is(err, testCase.wantError)) {
				t.Fatal("查询故障或异常记录返回了部分结果", err)
			}
			if err := pool.PingContext(context.Background()); err != nil {
				t.Fatal("查询关闭了共享连接池", err)
			}
		})
	}
}

func TestListUserSessionsInput(t *testing.T) {
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
		name     string
		ctx      context.Context
		userID   uint64
		pageSize int
		want     error
	}{
		{name: "nil context", userID: 7, pageSize: 1},
		{name: "zero user", ctx: context.Background(), pageSize: 1},
		{name: "zero page size", ctx: context.Background(), userID: 7},
		{name: "negative page size", ctx: context.Background(), userID: 7, pageSize: -1},
		{name: "excessive page size", ctx: context.Background(), userID: 7, pageSize: 101},
		{name: "integer limit", ctx: context.Background(), userID: 7, pageSize: int(^uint(0) >> 1)},
		{name: "canceled", ctx: canceled, userID: 7, pageSize: 1, want: context.Canceled},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			page, err := store.ListUserSessions(testCase.ctx, testCase.userID, 0, testCase.pageSize)
			if page != nil || err == nil || (testCase.want != nil && !errors.Is(err, testCase.want)) || connector.connections.Load() != 0 {
				t.Fatal("无效参数或已取消请求访问了数据库", err)
			}
		})
	}
}

func TestListUserSessionsCancellation(t *testing.T) {
	for _, phase := range []string{"caller deadline", "default deadline", "after result"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if phase == "caller deadline" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 30*time.Millisecond)
				defer stop()
			}
			connector := &deviceConnector{accountConnector: accountConnector{query: func(queryContext context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
				if phase == "after result" {
					cancel()
					return &userSessionRows{}, nil
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
			page, err := store.ListUserSessions(ctx, 7, 0, 1)
			want := error(context.DeadlineExceeded)
			if phase == "after result" {
				want = context.Canceled
			}
			if page != nil || !errors.Is(err, want) {
				t.Fatal("查询取消后返回了列表或丢失原因", err)
			}
		})
	}
}
