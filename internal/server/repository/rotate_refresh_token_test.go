package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// refreshRotationTestStore 复用已有事务驱动，在真实入口中绑定会话副本。
func refreshRotationTestStore(t *testing.T, verified domain.VerifiedRefreshSession, script *loginTransactionScript) *Store {
	t.Helper()
	script.status = "ACTIVE"
	script.sessionQuery = func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
		return &lockedRefreshRows{values: []driver.Value{
			strconv.FormatUint(verified.Session.ID, 10), strconv.FormatUint(verified.Session.UserID, 10),
			strconv.FormatUint(verified.Session.DeviceID, 10), verified.TokenHash[:],
			strconv.FormatUint(verified.TokenGeneration, 10), verified.Session.ExpiresAt, nil,
		}}, nil
	}
	return loginTransactionTestStore(t, script)
}

func TestRotateRefreshTokenResults(t *testing.T) {
	writeFailure := errors.New("fixture write failure")
	commitFailure := errors.New("fixture commit failure")
	duplicate := &mysql.MySQLError{Number: 1062, Message: "private duplicate hash"}
	foreignKey := &mysql.MySQLError{Number: 1452, Message: "fixture foreign key"}
	for _, testCase := range []struct {
		name                               string
		generation                         uint64
		rows                               int64
		writeError, commitError, wantError error
		zeroHash                           bool
	}{
		{name: "rotated", generation: 1, rows: 1},
		{name: "subsequent generation", generation: 41, rows: 1},
		{name: "maximum next generation", generation: ^uint64(0) - 1, rows: 1},
		{name: "zero fixed length hash", generation: 1, rows: 1, zeroHash: true},
		{name: "old state already consumed", generation: 1, wantError: domain.ErrInvalidRefreshToken},
		{name: "unexpected row count", generation: 1, rows: 2, wantError: domain.ErrInvalidSessionData},
		{name: "digest conflict", generation: 1, writeError: duplicate, wantError: domain.ErrRefreshTokenConflict},
		{name: "wrapped digest conflict", generation: 1, writeError: fmt.Errorf("wrapped: %w", duplicate), wantError: domain.ErrRefreshTokenConflict},
		{name: "database failure", generation: 1, writeError: writeFailure, wantError: writeFailure},
		{name: "foreign key failure", generation: 1, writeError: foreignKey, wantError: foreignKey},
		{name: "outer commit failure", generation: 1, rows: 1, commitError: commitFailure, wantError: commitFailure},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			verified, _ := refreshTransactionFixture()
			verified.TokenGeneration = testCase.generation
			newHash := sha256.Sum256([]byte("fixture new refresh"))
			if testCase.zeroHash {
				newHash = [sha256.Size]byte{}
			}
			var callbackContext context.Context
			script := &loginTransactionScript{commitError: testCase.commitError}
			script.rotate = func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
				expected := "UPDATE auth_sessions SET refresh_token_hash = ?, token_generation = ?, updated_at = UTC_TIMESTAMP(6) WHERE user_id = ? AND device_id = ? AND id = ? AND refresh_token_hash = ? AND token_generation = ? AND revoked_at IS NULL AND expires_at > ?"
				if statement != expected || len(arguments) != 8 {
					t.Fatal("更新字段或条件错误")
				}
				if !bytes.Equal(arguments[0].Value.([]byte), newHash[:]) || arguments[1].Value != verified.TokenGeneration+1 ||
					arguments[2].Value != verified.Session.UserID || arguments[3].Value != verified.Session.DeviceID ||
					arguments[4].Value != verified.Session.ID || !bytes.Equal(arguments[5].Value.([]byte), verified.TokenHash[:]) ||
					arguments[6].Value != verified.TokenGeneration {
					t.Error("新旧摘要、代次或归属参数未完整绑定")
				}
				checkedAt, valid := arguments[7].Value.(time.Time)
				if !valid || checkedAt.Location() != time.UTC || !checkedAt.Before(verified.Session.ExpiresAt) {
					t.Error("有效期比较未使用当前 UTC 时间")
				}
				parentDeadline, _ := callbackContext.Deadline()
				updateDeadline, exists := ctx.Deadline()
				if !exists || !updateDeadline.Equal(parentDeadline) {
					t.Error("更新未服从已有事务期限")
				}
				return driver.RowsAffected(testCase.rows), testCase.writeError
			}
			store := refreshRotationTestStore(t, verified, script)
			var prepared *domain.VerifiedRefreshSession
			err := store.WithRefreshUserTransaction(context.Background(), verified, func(ctx context.Context, txStore *Store, current domain.VerifiedRefreshSession) error {
				callbackContext = ctx
				// 改动回调参数副本，确认轮换归属取自 Store 绑定的锁内资料。
				current.Session.UserID = 999
				var err error
				prepared, err = txStore.RotateRefreshToken(ctx, newHash)
				return err
			})
			if testCase.wantError == nil {
				if err != nil || prepared == nil || prepared.Session != verified.Session || prepared.TokenHash != newHash || prepared.TokenGeneration != verified.TokenGeneration+1 {
					t.Fatal("轮换结果错误", err)
				}
			} else if !errors.Is(err, testCase.wantError) || (testCase.commitError == nil && prepared != nil) {
				t.Fatal("失败结果或错误原因不正确", err)
			}
			if errors.Is(err, domain.ErrRefreshTokenConflict) && strings.Contains(err.Error(), "private") {
				t.Error("摘要冲突错误泄漏驱动信息")
			}
			wantSequence := "begin,lock,device,session,rotate,rollback"
			if testCase.wantError == nil || testCase.commitError != nil {
				wantSequence = "begin,lock,device,session,rotate,commit"
			}
			if script.sequence() != wantSequence {
				t.Fatal("轮换事务顺序错误", script.sequence())
			}
		})
	}
}

func TestRotateRefreshTokenBoundary(t *testing.T) {
	for _, name := range []string{"ordinary store", "bound store without transaction", "login transaction", "nil context", "same hash", "generation overflow", "expiry after lock", "cancelled", "invalid bound owner"} {
		t.Run(name, func(t *testing.T) {
			verified, _ := refreshTransactionFixture()
			if name == "generation overflow" {
				verified.TokenGeneration = ^uint64(0)
			}
			script := &loginTransactionScript{}
			store := refreshRotationTestStore(t, verified, script)
			newHash := sha256.Sum256([]byte("fixture new refresh"))
			action := func(ctx context.Context, txStore *Store, _ domain.VerifiedRefreshSession) error {
				var want error
				switch name {
				case "nil context":
					ctx = nil
				case "same hash":
					newHash = verified.TokenHash
				case "generation overflow", "invalid bound owner":
					want = domain.ErrInvalidSessionData
					if name == "invalid bound owner" {
						txStore.loginUserID = 8
					}
				case "expiry after lock":
					// 仅修改测试持有的私有副本，模拟锁定后跨过截止时刻。
					txStore.refreshSession.Session.ExpiresAt = time.Now().Add(-time.Second)
					want = domain.ErrSessionExpired
				case "cancelled":
					cancelled, cancel := context.WithCancel(ctx)
					cancel()
					ctx, want = cancelled, context.Canceled
				}
				result, err := txStore.RotateRefreshToken(ctx, newHash)
				if result != nil || err == nil || (want != nil && !errors.Is(err, want)) {
					t.Error("无效轮换未按约定拒绝", err)
				}
				return err
			}
			var err error
			switch name {
			case "ordinary store":
				err = action(context.Background(), store, verified)
			case "bound store without transaction":
				candidate := &Store{database: store.database, loginUserID: 7, refreshSession: &verified}
				err = action(context.Background(), candidate, verified)
			case "login transaction":
				err = store.WithLoginUserTransaction(context.Background(), 7, func(ctx context.Context, txStore *Store) error {
					return action(ctx, txStore, verified)
				})
			default:
				err = store.WithRefreshUserTransaction(context.Background(), verified, action)
			}
			if err == nil || strings.Contains(script.sequence(), "rotate") || strings.Contains(script.sequence(), "insert") || strings.Contains(script.sequence(), "commit") {
				t.Fatal("无效轮换执行了 SQL 写入或提交")
			}
		})
	}
}

func TestRotateRefreshTokenCancellation(t *testing.T) {
	for _, stage := range []string{"during update", "after update"} {
		t.Run(stage, func(t *testing.T) {
			verified, _ := refreshTransactionFixture()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			script := &loginTransactionScript{}
			script.rotate = func(updateContext context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
				if stage == "during update" {
					<-updateContext.Done()
					return nil, updateContext.Err()
				}
				cancel()
				return driver.RowsAffected(1), nil
			}
			store := refreshRotationTestStore(t, verified, script)
			var result *domain.VerifiedRefreshSession
			err := store.WithRefreshUserTransaction(ctx, verified, func(txContext context.Context, txStore *Store, _ domain.VerifiedRefreshSession) error {
				var err error
				result, err = txStore.RotateRefreshToken(txContext, sha256.Sum256([]byte("fixture next")))
				return err
			})
			want := context.Canceled
			if stage == "during update" {
				want = context.DeadlineExceeded
			}
			if result != nil || !errors.Is(err, want) || strings.Contains(script.sequence(), "commit") {
				t.Fatal("更新取消后仍返回结果或提交", err)
			}
			select {
			case <-script.rolledBack:
			case <-time.After(time.Second):
				t.Fatal("取消后没有回滚")
			}
		})
	}
}
