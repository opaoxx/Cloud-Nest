package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type uploadPartSigningTaskRows struct{ *idempotencyRecordRows }

func (*uploadPartSigningTaskRows) Columns() []string {
	return append((&uploadTaskReadRows{}).Columns(), "database_time")
}

func TestFindUserUploadTaskForPartSigningResults(t *testing.T) {
	// 1. 在复用任务映射的基础上，验证同一次查询带回时钟或整体失败。
	want, values := uploadTaskReadFixture()
	databaseTime := time.Date(2026, 11, 21, 20, 0, 0, 123456000, time.FixedZone("fixture", 8*3600))
	failure := errors.New("fixture query failure")
	for _, test := range []struct {
		name    string
		clock   driver.Value
		missing bool
		fault   error
		cancel  bool
		want    error
	}{
		{name: "task and UTC clock", clock: databaseTime},
		{name: "missing task", missing: true, want: domain.ErrUploadTaskNotFound},
		{name: "missing clock", want: domain.ErrInvalidUploadTaskData},
		{name: "invalid clock precision", clock: databaseTime.Add(time.Nanosecond), want: domain.ErrInvalidUploadTaskData},
		{name: "query fault", fault: failure, want: failure},
		{name: "cancelled after query", clock: databaseTime, cancel: true, want: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			connector := &deviceConnector{accountConnector: accountConnector{query: func(queryContext context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				calls++
				expected := "SELECT " + strings.Join((&uploadTaskReadRows{}).Columns(), ",") +
					",UTC_TIMESTAMP(6) AS database_time FROM upload_tasks WHERE user_id = ? AND id = ? LIMIT ?"
				statement = strings.Join(strings.Fields(strings.ReplaceAll(statement, "`", "")), " ")
				if statement != expected || len(arguments) != 3 || arguments[0].Value != want.UserID ||
					arguments[1].Value != want.ID || arguments[2].Value != int64(1) {
					t.Error("签发读取未使用单条按归属查询", statement, arguments)
				}
				if deadline, ok := queryContext.Deadline(); !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
					t.Error("签发读取未设置查询期限")
				}
				var row []driver.Value
				if !test.missing {
					row = append(append([]driver.Value(nil), values...), test.clock)
				}
				if test.cancel {
					cancel()
				}
				return &uploadPartSigningTaskRows{&idempotencyRecordRows{values: row}}, test.fault
			}}}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}

			// 2. 任何错误都不交付部分记录，正常时返回原任务事实和 UTC。
			task, now, err := store.FindUserUploadTaskForPartSigning(ctx, want.UserID, want.ID)
			if calls != 1 || !errors.Is(err, test.want) {
				t.Fatal("签发读取的查询次数或结果不符", calls, err)
			}
			if test.want != nil {
				if task != nil || !now.IsZero() {
					t.Fatal("失败时返回了部分数据")
				}
			} else if !reflect.DeepEqual(task, &want) || !now.Equal(databaseTime) || now.Location() != time.UTC {
				t.Fatal("任务内容或数据库时间未完整读回", now)
			}
		})
	}
}
