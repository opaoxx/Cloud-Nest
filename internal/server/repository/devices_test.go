package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// deviceConnector 复用账号查询测试的连接器，补充真实 MySQL 驱动支持的 uint64 参数。
type deviceConnector struct{ accountConnector }

func (connector *deviceConnector) Connect(ctx context.Context) (driver.Conn, error) {
	connection, err := connector.accountConnector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &deviceConnection{accountConnection: connection.(*accountConnection)}, nil
}

type deviceConnection struct{ *accountConnection }

func (*deviceConnection) CheckNamedValue(value *driver.NamedValue) error {
	if _, unsigned := value.Value.(uint64); unsigned {
		return nil
	}
	return driver.ErrSkip
}

// deviceRows 只提供本轮六列只读查询的结果，不模拟写入或事务。
type deviceRows struct {
	records [][]driver.Value
	next    int
}

func (*deviceRows) Columns() []string {
	return []string{"id", "user_id", "installation_id", "name", "platform", "revoked_at"}
}
func (*deviceRows) Close() error { return nil }
func (rows *deviceRows) Next(destination []driver.Value) error {
	if rows.next == len(rows.records) {
		return io.EOF
	}
	copy(destination, rows.records[rows.next])
	rows.next++
	return nil
}

func TestFindLoginDeviceResultsAndOwnerBinding(t *testing.T) {
	const installationID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	revokedAt := time.Date(2026, 11, 10, 2, 30, 0, 123456000, time.UTC)
	base := domain.LoginDevice{ID: 11, UserID: 7, InstallationID: installationID, Name: "学习电脑", Platform: "WINDOWS"}
	revoked, otherOwner, largeID := base, base, base
	revoked.RevokedAt = &revokedAt
	otherOwner.ID, otherOwner.UserID = 22, 9
	largeID.ID, largeID.UserID = ^uint64(0), ^uint64(0)
	queryFailure := errors.New("private-database-failure")
	for _, testCase := range []struct {
		name                     string
		userID                   uint64
		installation             string
		records                  [][]driver.Value
		queryError               error
		want                     *domain.LoginDevice
		wantMissing, wantFailure bool
	}{
		{name: "existing null revocation", userID: 7, installation: installationID, records: [][]driver.Value{{int64(11), int64(7), installationID, "学习电脑", "WINDOWS", nil}}, want: &base},
		{name: "revoked still returned", userID: 7, installation: installationID, records: [][]driver.Value{{int64(11), int64(7), installationID, "学习电脑", "WINDOWS", revokedAt}}, want: &revoked},
		{name: "same installation another owner", userID: 9, installation: installationID, records: [][]driver.Value{{int64(22), int64(9), installationID, "学习电脑", "WINDOWS", nil}}, want: &otherOwner},
		{name: "another owner has no device", userID: 8, installation: installationID, wantMissing: true},
		{name: "unsigned ids preserved", userID: ^uint64(0), installation: installationID, records: [][]driver.Value{{"18446744073709551615", "18446744073709551615", installationID, "学习电脑", "WINDOWS", nil}}, want: &largeID},
		{name: "unknown installation", userID: 7, installation: "private-missing-installation", wantMissing: true},
		{name: "empty installation stays filtered", userID: 7, wantMissing: true},
		{name: "injection stays parameter", userID: 7, installation: "' OR 1=1 -- private-value", wantMissing: true},
		{name: "query failure", userID: 7, installation: installationID, queryError: queryFailure, wantFailure: true},
		{name: "scan failure", userID: 7, installation: installationID, records: [][]driver.Value{{"not-an-id", int64(7), installationID, "学习电脑", "WINDOWS", nil}}, wantFailure: true},
		{name: "invalid timestamp", userID: 7, installation: installationID, records: [][]driver.Value{{int64(11), int64(7), installationID, "学习电脑", "WINDOWS", "private-invalid-time"}}, wantFailure: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			queryCount := 0
			connector := &deviceConnector{accountConnector: accountConnector{query: func(ctx context.Context, query string, arguments []driver.NamedValue) (driver.Rows, error) {
				queryCount++
				// 必须是同时绑定归属与安装标识的六列只读查询，不应追加 revoked_at 过滤。
				if strings.ReplaceAll(query, "`", "") != "SELECT id,user_id,installation_id,name,platform,revoked_at FROM user_devices WHERE user_id = ? AND installation_id = ? LIMIT ?" || len(arguments) != 3 || arguments[0].Value != testCase.userID || arguments[1].Value != testCase.installation || arguments[2].Value != int64(1) {
					t.Fatal("SQL 投影、用户归属或绑定参数不正确")
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) > 5*time.Second {
					t.Error("查询没有 5 秒期限")
				}
				if testCase.queryError != nil {
					return nil, testCase.queryError
				}
				return &deviceRows{records: testCase.records}, nil
			}}}
			connectionPool := sql.OpenDB(connector)
			defer connectionPool.Close()
			store, err := NewStore(connectionPool)
			if err != nil {
				t.Fatal(err)
			}
			device, err := store.FindLoginDeviceByInstallation(context.Background(), testCase.userID, testCase.installation)
			if queryCount != 1 {
				t.Fatal("查询没有执行或发生了额外查询")
			}
			if testCase.wantMissing {
				if device != nil || !errors.Is(err, domain.ErrDeviceNotFound) {
					t.Fatal("未命中没有返回设备不存在")
				}
				return
			}
			if testCase.wantFailure {
				if device != nil || err == nil || errors.Is(err, domain.ErrDeviceNotFound) {
					t.Fatal("故障被误报为未命中或返回部分设备")
				}
				if testCase.queryError != nil && !errors.Is(err, queryFailure) {
					t.Fatal("查询故障原因丢失")
				}
				return
			}
			if err != nil || !reflect.DeepEqual(device, testCase.want) {
				t.Fatalf("设备或可空时间映射错误: %+v, error=%v", device, err)
			}
			if err := connectionPool.PingContext(context.Background()); err != nil {
				t.Fatal("查询关闭了共享池")
			}
		})
	}
}

func TestFindLoginDeviceInputAndCancellation(t *testing.T) {
	connector := &deviceConnector{}
	connectionPool := sql.OpenDB(connector)
	defer connectionPool.Close()
	store, err := NewStore(connectionPool)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, testCase := range []struct {
		name   string
		ctx    context.Context
		userID uint64
		want   error
	}{
		{name: "nil context", userID: 7},
		{name: "zero user", ctx: context.Background()},
		{name: "canceled", ctx: ctx, userID: 7, want: context.Canceled},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			device, err := store.FindLoginDeviceByInstallation(testCase.ctx, testCase.userID, "private-installation")
			if device != nil || err == nil || (testCase.want != nil && !errors.Is(err, testCase.want)) || connector.connections.Load() != 0 {
				t.Fatal("无效调用仍访问了数据库")
			}
			if strings.Contains(err.Error(), "private-") {
				t.Fatal("输入错误回显原始标识")
			}
		})
	}
}

func TestFindLoginDeviceTimeout(t *testing.T) {
	for _, callerTimeout := range []time.Duration{50 * time.Millisecond, 0} {
		name := "default five seconds"
		if callerTimeout > 0 {
			name = "caller shorter deadline"
		}
		t.Run(name, func(t *testing.T) {
			connector := &deviceConnector{accountConnector: accountConnector{query: func(ctx context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			}}}
			connectionPool := sql.OpenDB(connector)
			defer connectionPool.Close()
			store, err := NewStore(connectionPool)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if callerTimeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, callerTimeout)
				defer cancel()
			}
			started := time.Now()
			device, err := store.FindLoginDeviceByInstallation(ctx, 7, "28ee758a-0727-42de-ae7d-3bb33155e148")
			elapsed := time.Since(started)
			if device != nil || !errors.Is(err, context.DeadlineExceeded) || elapsed > 8*time.Second || (callerTimeout == 0 && elapsed < 4*time.Second) || (callerTimeout > 0 && elapsed > 2*time.Second) {
				t.Fatalf("期限处理错误 elapsed=%s error=%v", elapsed, err)
			}
		})
	}
}

func TestFindLoginDeviceIntegration(t *testing.T) {
	// 1. 只读查询现有开发库，不为测试创建设备或其他业务记录。
	if os.Getenv("CLOUDNEST_DEVICE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_DEVICE_TEST=1 后执行真实设备只读查询")
	}
	databaseConfig, err := config.LoadDatabaseConfig()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	connectionPool, err := OpenMySQL(ctx, databaseConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer connectionPool.Close()
	store, err := NewStore(connectionPool)
	if err != nil {
		t.Fatal(err)
	}
	account, err := store.FindLoginAccountByUsername(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	installationID := uuid.NewString()
	for _, ownerID := range []uint64{account.ID, ^uint64(0)} {
		if device, err := store.FindLoginDeviceByInstallation(ctx, ownerID, installationID); device != nil || !errors.Is(err, domain.ErrDeviceNotFound) {
			t.Fatal("未命中或无符号用户参数处理错误")
		}
	}

	// 2. 若已有设备，以独立 SQL 基线核对六列映射；没有则明确记录正例覆盖边界。
	var baseline domain.LoginDevice
	var revokedAt sql.NullTime
	err = connectionPool.QueryRowContext(ctx, "SELECT id,user_id,installation_id,name,platform,revoked_at FROM user_devices ORDER BY id LIMIT 1").Scan(&baseline.ID, &baseline.UserID, &baseline.InstallationID, &baseline.Name, &baseline.Platform, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		t.Log("当前库暂无设备：真实查询仅覆盖未命中/故障，正常和撤销记录映射由单元测试覆盖")
	} else {
		if err != nil {
			t.Fatal(err)
		}
		if revokedAt.Valid {
			baseline.RevokedAt = &revokedAt.Time
		}
		device, err := store.FindLoginDeviceByInstallation(ctx, baseline.UserID, baseline.InstallationID)
		if err != nil || !reflect.DeepEqual(device, &baseline) {
			t.Fatal("真实设备六列与独立只读基线不一致")
		}
		t.Log("已有设备六列与独立只读基线一致")
	}

	// 3. 关闭测试自己的池后，故障不能被映射为不存在。
	if err := connectionPool.Close(); err != nil {
		t.Fatal(err)
	}
	if device, err := store.FindLoginDeviceByInstallation(ctx, account.ID, installationID); device != nil || err == nil || errors.Is(err, domain.ErrDeviceNotFound) {
		t.Fatal("关闭池后的故障分类错误")
	}
}
