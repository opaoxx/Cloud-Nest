package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm/logger"
)

// accountConnector 只实现本轮查询需要的 SQL 测试边界，不引入额外 mock 依赖。
type accountConnector struct {
	query       func(context.Context, string, []driver.NamedValue) (driver.Rows, error)
	connections atomic.Int64
	pings       atomic.Int64
}

func (connector *accountConnector) Connect(context.Context) (driver.Conn, error) {
	connector.connections.Add(1)
	return &accountConnection{connector: connector}, nil
}
func (connector *accountConnector) Driver() driver.Driver { return accountDriver{} }

type accountDriver struct{}

func (accountDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("测试仅允许 Connector 创建连接")
}

type accountConnection struct{ connector *accountConnector }

func (*accountConnection) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("查询不应走测试外的 Prepare")
}
func (*accountConnection) Begin() (driver.Tx, error) {
	return nil, errors.New("只读查询不应开启事务")
}
func (*accountConnection) Close() error { return nil }
func (connection *accountConnection) Ping(ctx context.Context) error {
	connection.connector.pings.Add(1)
	return ctx.Err()
}
func (connection *accountConnection) QueryContext(ctx context.Context, query string, arguments []driver.NamedValue) (driver.Rows, error) {
	if connection.connector.query == nil {
		return nil, errors.New("构造 Store 不应发起隐式查询")
	}
	return connection.connector.query(ctx, query, arguments)
}

type accountRows struct {
	records [][]driver.Value
	next    int
}

func (*accountRows) Columns() []string { return []string{"id", "password_hash", "status"} }
func (*accountRows) Close() error      { return nil }
func (rows *accountRows) Next(destination []driver.Value) error {
	if rows.next == len(rows.records) {
		return io.EOF
	}
	copy(destination, rows.records[rows.next])
	rows.next++
	return nil
}

func TestNewStoreReusesPoolWithoutNetwork(t *testing.T) {
	// 1. 没有连接池应明确失败；已有池构造后仍不触发连接、版本查询或 Ping。
	if store, err := NewStore(nil); store != nil || err == nil {
		t.Fatal("nil 连接池被接受")
	}
	connector := &accountConnector{}
	connectionPool := sql.OpenDB(connector)
	defer connectionPool.Close()
	store, err := NewStore(connectionPool)
	if err != nil {
		t.Fatal(err)
	}
	wrappedPool, err := store.database.DB()
	if err != nil || wrappedPool != connectionPool || connector.connections.Load() != 0 || connector.pings.Load() != 0 {
		t.Fatal("Store 创建了额外池或触发隐式网络请求")
	}
	if store.database.Config.Logger != logger.Discard {
		t.Fatal("没有关闭原始 SQL 日志")
	}
}

func TestFindLoginAccountResultsAndBinding(t *testing.T) {
	// 1. 验证查到/未找到/禁用/故障，所有输入始终作为参数绑定。
	queryFailure := errors.New("private-database-failure")
	for _, testCase := range []struct {
		name, username            string
		records                   [][]driver.Value
		queryError                error
		wantNotFound, wantFailure bool
		wantID                    uint64
		wantStatus                string
	}{
		{name: "active account", username: "root", records: [][]driver.Value{{int64(1), "private-bcrypt-hash", "ACTIVE"}}, wantID: 1, wantStatus: "ACTIVE"},
		{name: "disabled returned for service decision", username: "disabled", records: [][]driver.Value{{int64(2), "private-bcrypt-hash", "DISABLED"}}, wantID: 2, wantStatus: "DISABLED"},
		{name: "unsigned bigint preserved", username: "large_id", records: [][]driver.Value{{"18446744073709551615", "private-bcrypt-hash", "ACTIVE"}}, wantID: ^uint64(0), wantStatus: "ACTIVE"},
		{name: "not found", username: "missing", wantNotFound: true},
		{name: "empty username still filtered", wantNotFound: true},
		{name: "sql injection stays a value", username: "root' OR 1=1 -- private-value", wantNotFound: true},
		{name: "query error", username: "root", queryError: queryFailure, wantFailure: true},
		{name: "scan error", username: "root", records: [][]driver.Value{{"not-an-id", "private-bcrypt-hash", "ACTIVE"}}, wantFailure: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			queryCount := 0
			connector := &accountConnector{query: func(ctx context.Context, query string, arguments []driver.NamedValue) (driver.Rows, error) {
				queryCount++
				// 只允许三列、一个用户名条件和一条结果，不能拼接输入或 SELECT *。
				normalizedSQL := strings.ReplaceAll(query, "`", "")
				if normalizedSQL != "SELECT id,password_hash,status FROM users WHERE username = ? LIMIT ?" || len(arguments) != 2 || arguments[0].Value != testCase.username || arguments[1].Value != int64(1) {
					t.Fatalf("SQL 或参数不符合只读查询约定: %s", query)
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) > 5*time.Second {
					t.Error("查询缺少 5 秒上限")
				}
				if testCase.queryError != nil {
					return nil, testCase.queryError
				}
				return &accountRows{records: testCase.records}, nil
			}}
			connectionPool := sql.OpenDB(connector)
			defer connectionPool.Close()
			store, err := NewStore(connectionPool)
			if err != nil {
				t.Fatal(err)
			}
			account, err := store.FindLoginAccountByUsername(context.Background(), testCase.username)
			if queryCount != 1 {
				t.Fatal("查询未执行或发生额外探测")
			}
			if testCase.wantNotFound {
				if account != nil || !errors.Is(err, domain.ErrAccountNotFound) {
					t.Fatal("未命中没有映射为账号不存在")
				}
				return
			}
			if testCase.wantFailure {
				if account != nil || err == nil || errors.Is(err, domain.ErrAccountNotFound) {
					t.Fatal("查询故障被当作不存在或返回部分账号")
				}
				if testCase.queryError != nil && !errors.Is(err, queryFailure) {
					t.Fatal("底层错误链丢失")
				}
				return
			}
			if err != nil || account == nil || account.ID != testCase.wantID || account.Status != testCase.wantStatus || account.PasswordHash != "private-bcrypt-hash" {
				t.Fatal("账号投影映射错误")
			}
			encoded, err := json.Marshal(account)
			if err != nil || strings.Contains(string(encoded), "private-bcrypt-hash") || strings.Contains(string(encoded), "PasswordHash") {
				t.Fatal("内部摘要被 JSON 序列化")
			}
			if err := connectionPool.PingContext(context.Background()); err != nil {
				t.Fatal("查询擅自关闭共享连接池")
			}
		})
	}
}

func TestFindLoginAccountCancellationAndTimeout(t *testing.T) {
	// 1. 预先取消不应发起查询，nil 上下文不应 panic。
	connector := &accountConnector{}
	connectionPool := sql.OpenDB(connector)
	defer connectionPool.Close()
	store, err := NewStore(connectionPool)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if account, err := store.FindLoginAccountByUsername(ctx, "root"); account != nil || !errors.Is(err, context.Canceled) || connector.connections.Load() != 0 {
		t.Fatal("已取消的请求仍查询数据库")
	}
	if account, err := store.FindLoginAccountByUsername(nil, "root"); account != nil || err == nil {
		t.Fatal("nil 上下文未明确报错")
	}

	// 2. 验证调用方较短期限及默认 5 秒，查询失败不返回账号。
	for _, callerTimeout := range []time.Duration{50 * time.Millisecond, 0} {
		name := "default five seconds"
		if callerTimeout > 0 {
			name = "caller shorter deadline"
		}
		t.Run(name, func(t *testing.T) {
			connector := &accountConnector{query: func(ctx context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			}}
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
			account, err := store.FindLoginAccountByUsername(ctx, "root")
			elapsed := time.Since(started)
			if account != nil || !errors.Is(err, context.DeadlineExceeded) || elapsed > 8*time.Second || (callerTimeout == 0 && elapsed < 4*time.Second) || (callerTimeout > 0 && elapsed > 2*time.Second) {
				t.Fatalf("查询期限错误 elapsed=%s err=%v", elapsed, err)
			}
		})
	}
}

func TestFindLoginAccountIntegration(t *testing.T) {
	// 1. 独立开关只读访问现有开发库，不创建账号、表或会话。
	if os.Getenv("CLOUDNEST_ACCOUNT_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_ACCOUNT_TEST=1 后执行真实账号只读查询")
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

	// 2. 不打印摘要，只检查现有 root 的最小字段可读。
	account, err := store.FindLoginAccountByUsername(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	if account.ID == 0 || account.Status != "ACTIVE" || !strings.HasPrefix(account.PasswordHash, "$2") {
		t.Fatal("现有 root 账号的登录字段不符合预期")
	}
	for _, username := range []string{"missing_" + strings.ReplaceAll(uuid.NewString(), "-", ""), "root' OR 1=1 --"} {
		if account, err := store.FindLoginAccountByUsername(ctx, username); account != nil || !errors.Is(err, domain.ErrAccountNotFound) {
			t.Fatal("不存在或注入形态输入意外查询到账号")
		}
	}
	// 3. 关闭原始连接池后查询应报故障，不伪装成账号不存在。
	if err := connectionPool.Close(); err != nil {
		t.Fatal(err)
	}
	if account, err := store.FindLoginAccountByUsername(ctx, "root"); account != nil || err == nil || errors.Is(err, domain.ErrAccountNotFound) {
		t.Fatal("关闭的共享池仍可查询或错误分类不正确")
	}
}
