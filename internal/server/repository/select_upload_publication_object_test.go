package repository

import (
	"context"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestSelectUploadPublicationObjectResults(t *testing.T) {
	readFailure := errors.New("fixture lookup failed")
	writeFailure := errors.New("fixture promotion failed")
	duplicateKey := &mysql.MySQLError{Number: 1062}
	for _, scenario := range []struct {
		name        string
		readCount   int
		readError   error
		writeError  error
		found       bool
		wrongDigest bool
		wantError   error
	}{
		{name: "reuse immediately", readCount: 1, found: true},
		{name: "reuse after duplicate key", readCount: 2, writeError: duplicateKey, found: true},
		{name: "missing after duplicate key", readCount: 2, writeError: duplicateKey, wantError: domain.ErrStoredObjectDedupConflict},
		{name: "retry lookup failure", readCount: 2, writeError: duplicateKey, readError: readFailure, wantError: readFailure},
		{name: "initial lookup failure", readCount: 1, readError: readFailure, wantError: readFailure},
		{name: "other promotion failure", readCount: 1, writeError: writeFailure, wantError: writeFailure},
		{name: "digest rejected before lookup", wrongDigest: true, wantError: domain.ErrUploadChecksumMismatch},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 建立当前暂存记录和可复用候选。
			candidate, values := storedObjectReadFixture()
			candidate.ID, values[0] = 29, int64(29)
			task, _, _, _, _ := uploadObjectReplacementFixture()
			task.Status = domain.UploadStatusVerifying
			task.ExpectedSHA256 = candidate.VerifiedSHA256
			staged := domain.StoredObject{
				ID: task.StagedObjectID, OwnerUserID: task.UserID, Status: domain.StoredObjectStatusVerifying,
				FileSize: task.FileSize, Provider: candidate.Provider, Bucket: candidate.Bucket, ObjectKey: "objects/staged",
			}
			digest := domain.UploadContentDigest{SizeBytes: task.FileSize, SHA256: *candidate.VerifiedSHA256}
			if scenario.wrongDigest {
				digest.SHA256[0] ^= 1
			}

			// 2. 注入查询结果和更新故障，验证重查次数与锁定范围。
			script := &loginTransactionScript{status: "ACTIVE"}
			lookups, promotions := 0, 0
			script.objectQuery = func(_ context.Context, statement string, _ []driver.NamedValue) (driver.Rows, error) {
				lookups++
				if lookups > scenario.readCount || !strings.Contains(statement, "dedup_hash = ?") || !strings.HasSuffix(statement, "FOR UPDATE") {
					t.Fatal("发生了额外查询或查询未锁定复用候选", statement)
				}
				if lookups == scenario.readCount && scenario.readError != nil {
					return nil, scenario.readError
				}
				var result []driver.Value
				if lookups == scenario.readCount && scenario.found {
					result = values
				}
				return &storedObjectReadRows{&idempotencyRecordRows{values: result}}, nil
			}
			script.promoteUploadObject = func(_ context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
				promotions++
				if promotions != 1 || lookups != 1 || scenario.writeError == nil {
					t.Fatal("发生了额外的暂存对象推进")
				}
				return nil, scenario.writeError
			}
			store := loginTransactionTestStore(t, script)

			// 3. 按公开选择入口检查结果和错误传递。
			ctx := context.Background()
			if object, err := store.SelectUploadPublicationObject(ctx, &task, &staged, digest); object != nil || err == nil {
				t.Fatal("事务外选择应被拒绝", err)
			}
			err := store.withActiveUserTransaction(ctx, task.UserID, func(txContext context.Context, txStore *Store) error {
				object, err := txStore.SelectUploadPublicationObject(txContext, &task, &staged, digest)
				if scenario.found && !reflect.DeepEqual(object, &candidate) || !scenario.found && object != nil {
					t.Fatal("发布对象选择结果错误", object, err)
				}
				return err
			})
			wantPromotions := 0
			if scenario.writeError != nil {
				wantPromotions = 1
			}
			if !errors.Is(err, scenario.wantError) || lookups != scenario.readCount || promotions != wantPromotions {
				t.Fatal("选择流程错误", err, script.sequence())
			}
		})
	}
}
