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

// userDeviceRows 提供设备列表的七列结果及读取中途故障。
type userDeviceRows struct {
	records       [][]driver.Value
	position      int
	terminalError error
}

func (*userDeviceRows) Columns() []string {
	return []string{"id", "user_id", "name", "platform", "created_at", "last_seen_at", "revoked_at"}
}
func (*userDeviceRows) Close() error { return nil }
func (rows *userDeviceRows) Next(destination []driver.Value) error {
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

func userDeviceValues(device domain.UserDevice) []driver.Value {
	values := []driver.Value{
		strconv.FormatUint(device.ID, 10), strconv.FormatUint(device.UserID, 10),
		device.Name, device.Platform, device.CreatedAt, nil, nil,
	}
	if device.LastSeenAt != nil {
		values[5] = *device.LastSeenAt
	}
	if device.RevokedAt != nil {
		values[6] = *device.RevokedAt
	}
	return values
}

func TestListUserDevicesResults(t *testing.T) {
	createdAt := time.Date(2026, 11, 13, 3, 0, 0, 123456000, time.UTC)
	seenAt, revokedAt := createdAt.Add(time.Minute), createdAt.Add(time.Hour)
	base := domain.UserDevice{ID: 11, UserID: 7, Name: "学习设备 📁", Platform: "WEB", CreatedAt: createdAt}
	older, oldest, history, seen, revoked, large, wrongOwner, zeroID := base, base, base, base, base, base, base, base
	older.ID, oldest.ID = 10, 9
	history.LastSeenAt, history.RevokedAt = &seenAt, &revokedAt
	seen.LastSeenAt, revoked.RevokedAt = &seenAt, &revokedAt
	large.ID, large.UserID = ^uint64(0), ^uint64(0)
	wrongOwner.UserID, zeroID.ID = 9, 0
	badID, badTime := userDeviceValues(base), userDeviceValues(base)
	badID[0], badTime[4] = "bad-id", "bad-time"
	var fullDevices []domain.UserDevice
	var fullRecords [][]driver.Value
	for index := 0; index < 101; index++ {
		device := base
		device.ID = uint64(201 - index)
		fullDevices = append(fullDevices, device)
		fullRecords = append(fullRecords, userDeviceValues(device))
	}
	queryFailure, rowFailure := errors.New("query fault"), errors.New("partial row fault")
	for _, testCase := range []struct {
		name                            string
		userID, beforeID                uint64
		pageSize                        int
		records                         [][]driver.Value
		queryError, rowError, wantError error
		want                            *domain.UserDevicePage
	}{
		{name: "empty page", userID: 7, pageSize: 2, want: &domain.UserDevicePage{Devices: []domain.UserDevice{}}},
		{name: "single device with null times", userID: 7, pageSize: 2, records: [][]driver.Value{userDeviceValues(base)}, want: &domain.UserDevicePage{Devices: []domain.UserDevice{base}}},
		{name: "exact page", userID: 7, pageSize: 2, records: [][]driver.Value{userDeviceValues(base), userDeviceValues(older)}, want: &domain.UserDevicePage{Devices: []domain.UserDevice{base, older}}},
		{name: "extra row indicates more", userID: 7, pageSize: 2, records: [][]driver.Value{userDeviceValues(base), userDeviceValues(older), userDeviceValues(oldest)}, want: &domain.UserDevicePage{Devices: []domain.UserDevice{base, older}, HasMore: true}},
		{name: "next page", userID: 7, beforeID: 12, pageSize: 1, records: [][]driver.Value{userDeviceValues(base), userDeviceValues(older)}, want: &domain.UserDevicePage{Devices: []domain.UserDevice{base}, HasMore: true}},
		{name: "history times preserved", userID: 7, pageSize: 2, records: [][]driver.Value{userDeviceValues(history)}, want: &domain.UserDevicePage{Devices: []domain.UserDevice{history}}},
		{name: "only last seen time", userID: 7, pageSize: 2, records: [][]driver.Value{userDeviceValues(seen)}, want: &domain.UserDevicePage{Devices: []domain.UserDevice{seen}}},
		{name: "only revoked time", userID: 7, pageSize: 2, records: [][]driver.Value{userDeviceValues(revoked)}, want: &domain.UserDevicePage{Devices: []domain.UserDevice{revoked}}},
		{name: "maximum page size", userID: 7, pageSize: 100, records: fullRecords, want: &domain.UserDevicePage{Devices: fullDevices[:100], HasMore: true}},
		{name: "full unsigned ids", userID: ^uint64(0), pageSize: 1, records: [][]driver.Value{userDeviceValues(large)}, want: &domain.UserDevicePage{Devices: []domain.UserDevice{large}}},
		{name: "maximum cursor", userID: 7, beforeID: ^uint64(0), pageSize: 2, records: [][]driver.Value{userDeviceValues(base)}, want: &domain.UserDevicePage{Devices: []domain.UserDevice{base}}},
		{name: "query failure", userID: 7, pageSize: 2, queryError: queryFailure, wantError: queryFailure},
		{name: "row failure discards partial page", userID: 7, pageSize: 2, records: [][]driver.Value{userDeviceValues(base)}, rowError: rowFailure, wantError: rowFailure},
		{name: "invalid id scan", userID: 7, pageSize: 2, records: [][]driver.Value{badID}},
		{name: "invalid timestamp scan", userID: 7, pageSize: 2, records: [][]driver.Value{badTime}},
		{name: "wrong owner", userID: 7, pageSize: 2, records: [][]driver.Value{userDeviceValues(wrongOwner)}, wantError: domain.ErrInvalidDeviceData},
		{name: "zero device id", userID: 7, pageSize: 2, records: [][]driver.Value{userDeviceValues(zeroID)}, wantError: domain.ErrInvalidDeviceData},
		{name: "invalid later row", userID: 7, pageSize: 2, records: [][]driver.Value{userDeviceValues(base), userDeviceValues(wrongOwner)}, wantError: domain.ErrInvalidDeviceData},
		{name: "invalid extra row", userID: 7, pageSize: 1, records: [][]driver.Value{userDeviceValues(base), userDeviceValues(wrongOwner)}, wantError: domain.ErrInvalidDeviceData},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 复用 SQL 测试连接器，核对七列、用户范围及有界分页。
			queries := 0
			connector := &deviceConnector{accountConnector: accountConnector{query: func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				queries++
				expected := "SELECT id,user_id,name,platform,created_at,last_seen_at,revoked_at FROM user_devices WHERE user_id = ?"
				wantedArguments := []driver.NamedValue{{Ordinal: 1, Value: testCase.userID}}
				if testCase.beforeID != 0 {
					expected += " AND id < ?"
					wantedArguments = append(wantedArguments, driver.NamedValue{Ordinal: 2, Value: testCase.beforeID})
				}
				expected += " ORDER BY id DESC LIMIT ?"
				wantedArguments = append(wantedArguments, driver.NamedValue{Ordinal: len(wantedArguments) + 1, Value: int64(testCase.pageSize + 1)})
				actual := strings.Join(strings.Fields(strings.ReplaceAll(statement, "\x60", "")), " ")
				if actual != expected || !reflect.DeepEqual(arguments, wantedArguments) {
					t.Error("设备列表的列、归属、排序或分页绑定错误", actual)
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) > 5*time.Second {
					t.Error("查询缺少期限")
				}
				return &userDeviceRows{records: testCase.records, terminalError: testCase.rowError}, testCase.queryError
			}}}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}

			// 2. 核对完整结果或失败，失败应交付空结果并保留原因。
			page, err := store.ListUserDevices(context.Background(), testCase.userID, testCase.beforeID, testCase.pageSize)
			if queries != 1 {
				t.Fatal("列表查询未恰好执行一次")
			}
			if testCase.want != nil {
				if err != nil || !reflect.DeepEqual(page, testCase.want) {
					t.Fatal("设备字段、空时间或分页映射错误", err)
				}
			} else if page != nil || err == nil || (testCase.wantError != nil && !errors.Is(err, testCase.wantError)) {
				t.Fatal("失败返回部分结果或丢失错误原因", err)
			}
			if err := pool.PingContext(context.Background()); err != nil {
				t.Fatal("查询关闭了共享连接池")
			}
		})
	}
}

func TestListUserDevicesInput(t *testing.T) {
	connector := &deviceConnector{}
	pool := sql.OpenDB(connector)
	defer pool.Close()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
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
		{name: "zero page", ctx: context.Background(), userID: 7},
		{name: "negative page", ctx: context.Background(), userID: 7, pageSize: -1},
		{name: "oversized page", ctx: context.Background(), userID: 7, pageSize: 101},
		{name: "integer maximum", ctx: context.Background(), userID: 7, pageSize: int(^uint(0) >> 1)},
		{name: "cancelled", ctx: cancelled, userID: 7, pageSize: 1, want: context.Canceled},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			page, err := store.ListUserDevices(testCase.ctx, testCase.userID, 0, testCase.pageSize)
			if page != nil || err == nil || (testCase.want != nil && !errors.Is(err, testCase.want)) || connector.connections.Load() != 0 {
				t.Fatal("无效参数或取消请求进入了数据库", err)
			}
		})
	}
}

func TestListUserDevicesCancellation(t *testing.T) {
	for _, phase := range []string{"caller deadline", "default deadline", "after result"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if phase == "caller deadline" {
				var release context.CancelFunc
				ctx, release = context.WithTimeout(ctx, 30*time.Millisecond)
				defer release()
			}
			connector := &deviceConnector{accountConnector: accountConnector{query: func(queryContext context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
				if phase == "after result" {
					cancel()
					return &userDeviceRows{}, nil
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
			page, err := store.ListUserDevices(ctx, 7, 0, 1)
			want := error(context.DeadlineExceeded)
			if phase == "after result" {
				want = context.Canceled
			}
			if page != nil || !errors.Is(err, want) {
				t.Fatal("取消后返回了页面或丢失原因", err)
			}
		})
	}
}
