package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type uploadVerificationCandidateRows struct {
	accountRows
	rowError error
}

func (*uploadVerificationCandidateRows) Columns() []string {
	return []string{"task_id", "user_id", "staged_object_id", "lease_version"}
}

func (rows *uploadVerificationCandidateRows) Next(destination []driver.Value) error {
	if rows.next == len(rows.records) && rows.rowError != nil {
		return rows.rowError
	}
	return rows.accountRows.Next(destination)
}

func TestListUploadVerificationCandidatesResults(t *testing.T) {
	failure := errors.New("fixture candidate query failure")
	first := []driver.Value{"19", "7", "23", "0"}
	large := []driver.Value{"18446744073709551615", "18446744073709551615", "18446744073709551615", "18446744073709551614"}
	for _, fixture := range []struct {
		name    string
		records [][]driver.Value
		want    []domain.UploadVerificationCandidate
		fail    error
	}{
		{"empty", nil, []domain.UploadVerificationCandidate{}, nil},
		{"complete unsigned range", [][]driver.Value{first, large}, []domain.UploadVerificationCandidate{
			{TaskID: 19, UserID: 7, StagedObjectID: 23, LeaseVersion: 0},
			{TaskID: math.MaxUint64, UserID: math.MaxUint64, StagedObjectID: math.MaxUint64, LeaseVersion: math.MaxUint64 - 1},
		}, nil},
		{"unordered", [][]driver.Value{large, first}, nil, domain.ErrInvalidUploadTaskData},
		{"duplicate", [][]driver.Value{first, first}, nil, domain.ErrInvalidUploadTaskData},
		{"zero owner", [][]driver.Value{{"19", "0", "23", "0"}}, nil, domain.ErrInvalidUploadTaskData},
		{"zero task", [][]driver.Value{{"0", "7", "23", "0"}}, nil, domain.ErrInvalidUploadTaskData},
		{"zero object", [][]driver.Value{{"19", "7", "0", "0"}}, nil, domain.ErrInvalidUploadTaskData},
		{"exhausted version", [][]driver.Value{{"19", "7", "23", "18446744073709551615"}}, nil, domain.ErrInvalidUploadTaskData},
		{"too many rows", [][]driver.Value{first, large, large}, nil, domain.ErrInvalidUploadTaskData},
		{"query failure", nil, nil, failure},
		{"partial read failure", [][]driver.Value{first}, nil, failure},
		{"canceled after query", [][]driver.Value{first}, nil, context.Canceled},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			connector := &deviceConnector{}
			connector.query = func(queryContext context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				// 1. 核对数据库时间边界、排序、批量和只读查询。
				projection, _, _ := strings.Cut(statement, " FROM ")
				if strings.ReplaceAll(projection, "`", "") != "SELECT id AS task_id,user_id,staged_object_id,lease_version" {
					t.Error("扫描 SQL 的列清单不符", projection)
				}
				for _, condition := range []string{
					"status = 'VERIFYING'", "upload_mode = 'SINGLE'",
					"expires_at > UTC_TIMESTAMP(6)", "next_attempt_at <= UTC_TIMESTAMP(6)", "lease_until <= UTC_TIMESTAMP(6)",
					"lease_version < ?", "ORDER BY id ASC LIMIT ?",
				} {
					if !strings.Contains(statement, condition) {
						t.Error("扫描 SQL 缺少候选条件", condition, statement)
					}
				}
				if !strings.HasPrefix(statement, "SELECT ") || strings.Contains(statement, "FOR UPDATE") ||
					len(arguments) != 2 || arguments[0].Value != uint64(math.MaxUint64) || arguments[1].Value != int64(2) {
					t.Error("扫描语句或绑定参数错误", statement, arguments)
				}
				if deadline, exists := queryContext.Deadline(); !exists || time.Until(deadline) > 5*time.Second {
					t.Error("候选查询缺少读期限")
				}
				rows := &uploadVerificationCandidateRows{accountRows: accountRows{records: fixture.records}}
				switch fixture.name {
				case "query failure":
					return nil, failure
				case "partial read failure":
					rows.rowError = failure
				case "canceled after query":
					cancel()
				}
				return rows, nil
			}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			got, err := store.ListUploadVerificationCandidates(ctx, 2)
			if !errors.Is(err, fixture.fail) || !reflect.DeepEqual(got, fixture.want) {
				t.Fatal("候选、错误或部分结果异常", got, err)
			}
		})
	}
}

func TestListUploadVerificationCandidatesInputs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	script := &loginTransactionScript{}
	store := revokeSessionTestStore(t, actor, script)
	for _, fixture := range []struct {
		store *Store
		ctx   context.Context
		limit int
	}{
		{nil, context.Background(), 1},
		{&Store{}, context.Background(), 1},
		{store, nil, 1},
		{store, context.Background(), 0},
		{store, context.Background(), 101},
		{store, ctx, 1},
	} {
		if got, err := fixture.store.ListUploadVerificationCandidates(fixture.ctx, fixture.limit); got != nil || err == nil {
			t.Fatal("无效查询被接受", got, err)
		}
	}
	if err := store.WithUserTransaction(context.Background(), actor, func(txContext context.Context, txStore *Store) error {
		if got, err := txStore.ListUploadVerificationCandidates(txContext, 1); got != nil || err == nil {
			t.Error("用户事务的跨账号扫描被接受", got, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
